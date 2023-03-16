package p2p

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/hashicorp/golang-lru/v2/simplelru"
	"golang.org/x/time/rate"

	"github.com/ethereum/go-ethereum/log"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"

	"github.com/ethereum-optimism/optimism/op-node/eth"
	"github.com/ethereum-optimism/optimism/op-node/rollup"
)

func PayloadByNumberProtocolID(l2ChainID *big.Int) protocol.ID {
	return protocol.ID(fmt.Sprintf("/opstack/%s/reqresp/payload_by_number/0.1.0", l2ChainID))
}

type requestHandlerFn func(ctx context.Context, log log.Logger, stream network.Stream)

func MakeStreamHandler(resourcesCtx context.Context, log log.Logger, fn requestHandlerFn) network.StreamHandler {
	return func(stream network.Stream) {
		// TODO metrics
		log := log.New("peer", stream.Conn().ID(), "remote", stream.Conn().RemoteMultiaddr())
		defer func() {
			if err := recover(); err != nil {
				log.Error("p2p server request handling panic", "err", err, "protocol", stream.Protocol())
			}
		}()
		defer stream.Close()
		fn(resourcesCtx, log, stream)
	}
}

type newStreamFn func(ctx context.Context, peerId peer.ID, protocolId ...protocol.ID) (network.Stream, error)

type receivePayload = func(ctx context.Context, from peer.ID, payload *eth.ExecutionPayload) error

type rangeRequest struct {
	start uint64
	end   eth.L2BlockRef
}

type syncResult struct {
	payload *eth.ExecutionPayload
	peer    peer.ID
}

type peerRequest struct {
	num uint64

	complete *atomic.Bool
}

// P2PSyncClient implements a reverse chain sync with a minimal interface:
// signal the desired range, and receive blocks within this range back.
// Through parent-hash verification, received blocks are all ensured to be part of the canonical chain at one point,
// but it is up to the user to organize and process the results further.
//
// For the sync-client to retrieve any data, peers must be added with AddPeer(id), and removed upon disconnect with RemovePeer(id).
// The client is started with Start(), and may be started before or after changing any peers.
//
// ### Stages
//
// The sync mechanism is implemented as following:
// - User sends range request: blocks on sync main loop (with ctx timeout)
// - Main loop processes range request (from high to low), dividing block requests by number between parallel peers.
//   - The high part of the range has a known block-hash, and is marked as trusted.
//   - Once there are no more peers available for buffering requests, we stop the range request processing.
//   - Every request buffered for a peer is tracked as in-flight, by block number.
//   - In-flight requests are not repeated
//   - Requests for data that's already in the quarantine are not repeated
//   - Data already in the quarantine that is trusted is attempted to be promoted.
//
// - Peers each have their own routine for processing requests.
//   - They fetch the requested block by number, parse and validate it, and then send it back to the main loop
//   - If peers fail to fetch or process it, or fail to send it back to the main loop within timeout,
//     then the doRequest returns an error. It then marks the in-flight request as completed.
//
// - Main loop receives results synchronously with the range requests
//   - The result is removed from in-flight tracker
//   - The result is added to the quarantine
//   - If we trust the hash, we try to promote the result.
//
// ### Concepts
//
// The main concepts are:
// - Quarantine: an LRU that stores the latest fetched block data, by hash as well as an extra index by number.
//
//   - Quarantine eviction: upon regular LRU eviction, or explicit removal (when we learn data is not canonical),
//     the sync result is removed from quarantine without being forwarded to the receiver.
//     The peer that provided the data may be down-scored for providing un-utilized data if the data
//     is not trusted during eviction.
//
// - Trusted data: data becomes trusted through 2 ways:
//   - The hash / parent-hash of the sync target is marked as trusted.
//   - The parent-hash of any promoted data is marked as trusted.
//
// - The trusted-data is maintained in LRU: we only care about the recent accessed blocks.
//
//   - Result promotion: content from the quarantine is "promoted" when we find the blockhash is trusted.
//     The data is removed from the quarantine, and forwarded to the receiver.
//
// ### Usage
//
// The user is expected to request the range of blocks between its existing chain head,
// and a trusted future block-hash as reference to sync towards.
// Upon receiving results from the sync-client, the user should adjust down its sync-target
// based on the received results, to avoid duplicating work when req-requesting an updated range.
// Range requests should still be repeated eventually however, as the sync client will give up on syncing a large range
// when it's too busy syncing.
//
// The rationale for this approach is that this sync mechanism is primarily intended
// for quickly filling gaps between an existing chain and a gossip chain, and not for very long block ranges.
// Syncing in the execution-layer (through snap-sync) is more appropriate for long ranges.
// If the user does sync a long range of blocks through this mechanism,
// it does end up traversing through the chain, but receives the blocks in reverse order.
// It is up to the user to persist the blocks for later processing, or drop & resync them if persistence is limited.
type P2PSyncClient struct {
	log log.Logger

	cfg *rollup.Config

	newStreamFn     newStreamFn
	payloadByNumber protocol.ID

	sync.Mutex
	// syncing worker per peer
	peers map[peer.ID]context.CancelFunc

	// trusted blocks are, or have been, canonical at one point.
	// Everything that's trusted is acceptable to pass to the sync receiver,
	// but we target to just sync the blocks of the latest canonical view of the chain.
	trusted *simplelru.LRU[common.Hash, struct{}]

	// quarantine is a LRU of untrusted results: blocks that could not be verified yet
	quarantine *simplelru.LRU[common.Hash, syncResult]
	// quarantineByNum indexes the quarantine contents by number.
	// No duplicates here, only the latest quarantine write is indexed.
	// This map is cleared upon evictions of items from the quarantine LRU
	quarantineByNum map[uint64]common.Hash

	// inFlight requests are not repeated
	inFlight map[uint64]*atomic.Bool

	requests     chan rangeRequest
	peerRequests chan peerRequest

	results chan syncResult

	resCtx    context.Context
	resCancel context.CancelFunc

	receivePayload receivePayload
	wg             sync.WaitGroup
}

func NewP2PSyncClient(log log.Logger, cfg *rollup.Config, newStream newStreamFn, rcv receivePayload) *P2PSyncClient {
	ctx, cancel := context.WithCancel(context.Background())

	c := &P2PSyncClient{
		log:             log,
		cfg:             cfg,
		newStreamFn:     newStream,
		payloadByNumber: PayloadByNumberProtocolID(cfg.L2ChainID),
		peers:           make(map[peer.ID]context.CancelFunc),
		quarantineByNum: make(map[uint64]common.Hash),
		inFlight:        make(map[uint64]*atomic.Bool),
		requests:        make(chan rangeRequest), // blocking
		peerRequests:    make(chan peerRequest, 128),
		results:         make(chan syncResult, 128),
		resCtx:          ctx,
		resCancel:       cancel,
		receivePayload:  rcv,
	}
	// never errors with positive LRU cache size
	// TODO: if we had an LRU based on on total payloads size, instead of payload count,
	//  we can safely buffer more data in the happy case.
	q, _ := simplelru.NewLRU[common.Hash, syncResult](100, c.onQuarantineEvict)
	c.quarantine = q
	trusted, _ := simplelru.NewLRU[common.Hash, struct{}](10000, nil)
	c.trusted = trusted
	return c
}

func (s *P2PSyncClient) Start() {
	s.wg.Add(1)
	go s.mainLoop()
}

func (s *P2PSyncClient) AddPeer(id peer.ID) {
	s.Lock()
	defer s.Unlock()
	if _, ok := s.peers[id]; ok {
		s.log.Warn("cannot register peer for sync duties, peer was already registered", "peer", id)
		return
	}
	s.wg.Add(1)
	// add new peer routine
	ctx, cancel := context.WithCancel(s.resCtx)
	s.peers[id] = cancel
	go s.peerLoop(ctx, id)
}

func (s *P2PSyncClient) RemovePeer(id peer.ID) {
	s.Lock()
	defer s.Unlock()
	cancel, ok := s.peers[id]
	if !ok {
		s.log.Warn("cannot remove peer from sync duties, peer was not registered", "peer", id)
		return
	}
	cancel() // once loop exits
	delete(s.peers, id)
}

func (s *P2PSyncClient) Close() error {
	s.resCancel()
	s.wg.Wait()
	return nil
}

func (s *P2PSyncClient) RequestL2Range(ctx context.Context, start, end eth.L2BlockRef) error {
	if end == (eth.L2BlockRef{}) {
		s.log.Debug("P2P sync client received range signal, but cannot sync open-ended chain: need sync target to verify blocks through parent-hashes", "start", start)
		return nil
	}
	// synchronize requests with the main loop for state access
	select {
	case s.requests <- rangeRequest{start: start.Number, end: end}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("too busy with P2P results/requests: %w", ctx.Err())
	}
}

const (
	maxRequestScheduling = time.Second * 3
	maxResultProcessing  = time.Second * 3
)

func (s *P2PSyncClient) mainLoop() {
	defer s.wg.Done()
	for {
		select {
		case req := <-s.requests:
			ctx, cancel := context.WithTimeout(s.resCtx, maxRequestScheduling)
			s.onRangeRequest(ctx, req)
			cancel()
		case res := <-s.results:
			ctx, cancel := context.WithTimeout(s.resCtx, maxResultProcessing)
			s.onResult(ctx, res)
			cancel()
		case <-s.resCtx.Done():
			s.log.Info("stopped P2P req-resp L2 block sync client")
			return
		}
	}
}

// onRangeRequest is exclusively called by the main loop, and has thus direct access to the request bookkeeping state.
// This function transforms requested block ranges into work for each peer.
func (s *P2PSyncClient) onRangeRequest(ctx context.Context, req rangeRequest) {
	// add req head to trusted set of blocks
	s.trusted.Add(req.end.Hash, struct{}{})
	s.trusted.Add(req.end.ParentHash, struct{}{})

	log := s.log.New("target", req.start, "end", req.end)

	// clean up the completed in-flight requests
	for k, v := range s.inFlight {
		if v.Load() {
			delete(s.inFlight, k)
		}
	}

	// Now try to fetch lower numbers than current end, to traverse back towards the updated start.
	for i := uint64(0); ; i++ {
		num := req.end.Number - 1 - i
		if num <= req.start {
			return
		}
		// check if we have something in quarantine already
		if h, ok := s.quarantineByNum[num]; ok {
			if s.trusted.Contains(h) { // if we trust it, try to promote it.
				s.tryPromote(h)
			}
			// Don't fetch things that we have a candidate for already.
			// We'll evict it from quarantine by finding a conflict, or if we sync enough other blocks
			continue
		}

		if _, ok := s.inFlight[num]; ok {
			continue // request still in flight
		}
		pr := peerRequest{num: num, complete: new(atomic.Bool)}

		log.Debug("Scheduling P2P block request", "num", num)
		// schedule number
		select {
		case s.peerRequests <- pr:
			s.inFlight[num] = pr.complete
		case <-ctx.Done():
			log.Info("did not schedule full P2P sync range", "current", num, "err", ctx.Err())
		default: // peers may all be busy processing requests already
			log.Info("no peers ready to handle block requests for more P2P requests for L2 block history", "current", num)
			return
		}
	}
}

func (s *P2PSyncClient) onQuarantineEvict(key common.Hash, value syncResult) {
	delete(s.quarantineByNum, uint64(value.payload.BlockNumber))
	if !s.trusted.Contains(key) {
		s.log.Debug("evicting untrusted payload from quarantine", "id", value.payload.ID(), "peer", value.peer)
		// TODO downscore peer for having provided us a bad block that never turned out to be canonical
	} else {
		s.log.Debug("evicting trusted payload from quarantine", "id", value.payload.ID(), "peer", value.peer)
	}
}

func (s *P2PSyncClient) tryPromote(h common.Hash) {
	parentRes, ok := s.quarantine.Get(h)
	if ok {
		// Simply reschedule the result, to get it (and possibly its parents) out of quarantine without recursion
		select {
		case s.results <- parentRes:
		default:
		}
	}
}

func (s *P2PSyncClient) promote(ctx context.Context, res syncResult) {
	if err := s.receivePayload(ctx, res.peer, res.payload); err != nil {
		s.log.Warn("failed to promote payload, receiver error", "err", err)
		return
	}
	s.trusted.Add(res.payload.BlockHash, struct{}{})
	if s.quarantine.Remove(res.payload.BlockHash) {
		s.log.Debug("promoted previously p2p-synced block from quarantine to main", "id", res.payload.ID())
	} else {
		s.log.Debug("promoted new p2p-synced block to main", "id", res.payload.ID())
	}

	// Try to promote the parent block too, if any: previous unverifiable data may now be canonical
	s.tryPromote(res.payload.ParentHash)

	// In case we don't have the parent, and what we have in quarantine is wrong,
	// clear what we buffered in favor of fetching something else.
	if h, ok := s.quarantineByNum[uint64(res.payload.BlockNumber)-1]; ok {
		s.quarantine.Remove(h)
	}
}

// onResult is exclusively called by the main loop, and has thus direct access to the request bookkeeping state.
// This function verifies if the result is canonical, and either promotes the result or moves the result into quarantine.
func (s *P2PSyncClient) onResult(ctx context.Context, res syncResult) {
	// Clean up the in-flight request, we have a result now.
	delete(s.inFlight, uint64(res.payload.BlockNumber))
	// Always put it in quarantine first. If promotion fails because the receiver is too busy, this functions as cache.
	s.quarantine.Add(res.payload.BlockHash, res)
	s.quarantineByNum[uint64(res.payload.BlockNumber)] = res.payload.BlockHash
	// If we know this block is canonical, then promote it
	if s.trusted.Contains(res.payload.BlockHash) {
		s.promote(ctx, res)
	}
}

// peerLoop for syncing from a single peer
func (s *P2PSyncClient) peerLoop(ctx context.Context, id peer.ID) {
	defer func() {
		s.Lock()
		delete(s.peers, id) // clean up
		s.wg.Done()
		s.Unlock()
		s.log.Debug("stopped syncing loop of peer", "id", id)
	}()

	log := s.log.New("peer", id)
	log.Info("Starting P2P sync client event loop")

	var rl rate.Limiter

	// allow 1 request per 10 ms
	rl.SetLimit(rate.Every(time.Millisecond * 10))
	rl.SetBurst(10) // and burst up to 10 items over that at any time

	for {
		// wait for peer to be available for more work
		if err := rl.WaitN(ctx, 1); err != nil {
			return
		}

		// once the peer is available, wait for a sync request.
		select {
		case pr := <-s.peerRequests:
			// We already established the peer is available w.r.t. rate-limiting,
			// and this is the only loop over this peer, so we can request now.
			err := s.doRequest(ctx, id, pr.num)
			if err != nil {
				// mark as complete if there's an error: we are not sending any result and can complete immediately.
				pr.complete.Store(true)
				log.Warn("failed p2p sync request", "num", pr.num, "err", err)
				// If we hit an error, then count it as many requests.
				// We'd like to avoid making more requests for a while, to back off.
				if err := rl.WaitN(ctx, 100); err != nil {
					return
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func (s *P2PSyncClient) doRequest(ctx context.Context, id peer.ID, n uint64) error {
	// check if peer is alive still.
	// if not, then reschedule the request, and exit the event loop

	// open stream to peer
	reqCtx, reqCancel := context.WithTimeout(ctx, time.Second*5)
	str, err := s.newStreamFn(reqCtx, id, s.payloadByNumber)
	reqCancel()
	if err != nil {
		return fmt.Errorf("failed to open stream: %w", err)
	}
	defer str.Close()
	// set write timeout (if available)
	_ = str.SetWriteDeadline(time.Now().Add(time.Second * 5))
	if err := binary.Write(str, binary.LittleEndian, n); err != nil {
		return fmt.Errorf("failed to write request (%d): %w", n, err)
	}
	if err := str.CloseWrite(); err != nil {
		return fmt.Errorf("failed to close writer side while making request: %w", err)
	}

	// set read timeout (if available)
	_ = str.SetReadDeadline(time.Now().Add(time.Second * 20))
	var res eth.ExecutionPayload
	var length uint32
	if err := binary.Read(str, binary.LittleEndian, &length); err != nil {
		return fmt.Errorf("failed to read response length: %w", err)
	}
	if length > maxGossipSize {
		return fmt.Errorf("length is too large: %d", length)
	}
	// TODO: snappy compression
	if err := res.UnmarshalSSZ(length, str); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}
	if err := str.CloseRead(); err != nil {
		return fmt.Errorf("failed to close reading side")
	}
	if err := verifyBlock(&res, n); err != nil {
		return fmt.Errorf("received execution payload is invalid: %w", err)
	}
	// TODO track duration it took to get it
	// TODO update scores
	select {
	case s.results <- syncResult{payload: &res, peer: id}:
	case <-ctx.Done():
		return fmt.Errorf("failed to process response, sync client is too busy: %w", err)
	}
	return nil
}

func verifyBlock(payload *eth.ExecutionPayload, expectedNum uint64) error {
	// verify L2 block
	if expectedNum != uint64(payload.BlockNumber) {
		return fmt.Errorf("received execution payload for block %d, but expected block %d", payload.BlockNumber, expectedNum)
	}
	actual, ok := payload.CheckBlockHash()
	if !ok { // payload itself contains bad block hash
		return fmt.Errorf("received execution payload for block %d with bad block hash %s, expected %s", expectedNum, payload.BlockHash, actual)
	}
	return nil
}

// StreamCtxFn provides a new context to use when handling stream requests
type StreamCtxFn func() context.Context

const (
	serverReadRequestTimeout = time.Second * 10
	serverWriteChunkTimeout  = time.Second * 10
	// after the rate-limit reservation hits the max throttle delay, give up on serving a request and just close the stream
	maxThrottleDelay                       = time.Second * 20
	globalServerBlocksRateLimit rate.Limit = 3
	globalServerBlocksBurst                = 2
	peerServerBlocksRateLimit   rate.Limit = 4
	peerServerBlocksBurst                  = 3
)

// peerStat maintains rate-limiting data of a peer that requests blocks from us.
type peerStat struct {
	// Requests tokenizes each request to sync
	Requests *rate.Limiter
}

type L2Chain interface {
	PayloadByNumber(ctx context.Context, number uint64) (*eth.ExecutionPayload, error)
}

type P2PReqRespServer struct {
	log log.Logger

	cfg *rollup.Config

	l2 L2Chain

	peerRateLimits *simplelru.LRU[peer.ID, *peerStat]
	peerStatsLock  sync.Mutex

	globalRequestsRL *rate.Limiter
}

// TODO: add interface argument to fetch payloads from engine with
func NewP2PReqRespServer(log log.Logger, cfg *rollup.Config, l2 L2Chain) *P2PReqRespServer {
	// We should never allow over 1000 different peers to churn through quickly,
	// so it's fine to prune rate-limit details past this.

	peerRateLimits, _ := simplelru.NewLRU[peer.ID, *peerStat](1000, nil)
	// 3 sync requests per second, with 2 burst
	globalRequestsRL := rate.NewLimiter(globalServerBlocksRateLimit, globalServerBlocksBurst)

	return &P2PReqRespServer{
		log:              log,
		cfg:              cfg,
		l2:               l2,
		peerRateLimits:   peerRateLimits,
		globalRequestsRL: globalRequestsRL,
	}
}

// HandleSyncRequest is a LibP2P stream handler function to register the L2 unsafe payloads alt-sync protocol.
//
// note that the same peer may open parallel streams
func (srv *P2PReqRespServer) HandleSyncRequest(ctx context.Context, log log.Logger, stream network.Stream) {
	peerId := stream.Conn().RemotePeer()

	// We wait as long as necessary; we throttle the peer instead of disconnecting,
	// unless the delay reaches a threshold that is unreasonable to wait for.
	ctx, cancel := context.WithTimeout(ctx, maxThrottleDelay)
	defer cancel()

	// take a token from the global rate-limiter,
	// to make sure there's not too much concurrent server work between different peers.
	if err := srv.globalRequestsRL.Wait(ctx); err != nil {
		log.Warn("timed out waiting for global sync rate limit", "err", err)
		return
	}

	// find rate limiting data of peer, or add otherwise
	srv.peerStatsLock.Lock()
	ps, _ := srv.peerRateLimits.Get(peerId)
	if ps == nil {
		ps = &peerStat{
			Requests: rate.NewLimiter(peerServerBlocksRateLimit, peerServerBlocksBurst),
		}
		srv.peerRateLimits.Add(peerId, ps)
		ps.Requests.Reserve() // count the hit, but make it delay the next request rather than immediately waiting
	} else {
		// Only wait if it's an existing peer, otherwise the instant rate-limit Wait call always errors.

		// If the requester thinks we're taking too long, then it's their problem and they can disconnect.
		// We'll disconnect ourselves only when failing to read/write,
		// if the work is invalid (range validation), or when individual sub tasks timeout.
		if err := ps.Requests.Wait(ctx); err != nil {
			log.Warn("timed out waiting for global sync rate limit", "err", err)
			return
		}
	}
	srv.peerStatsLock.Unlock()

	// Set read deadline, if available
	_ = stream.SetReadDeadline(time.Now().Add(serverReadRequestTimeout))

	// Read the request
	var req uint64
	if err := binary.Read(stream, binary.LittleEndian, &req); err != nil {
		log.Debug("failed to read requested block number", "err", err)
		return
	}
	if err := stream.CloseRead(); err != nil {
		log.Debug("failed to close reading-side of a P2P sync request call", "err", err)
		return
	}

	log = log.New("num", req)

	// Check the request is within the expected range of blocks
	if req < srv.cfg.Genesis.L2.Number {
		log.Warn("cannot serve request for L2 block before genesis")
		return
	}
	max, err := srv.cfg.TargetBlockNumber(uint64(time.Now().Unix()))
	if err != nil {
		log.Warn("cannot serve request for L2 block before genesis", "err", err)
		return
	}
	if req > max {
		log.Warn("cannot serve request for L2 block after max expected block", "max", max)
		return
	}
	payload, err := srv.l2.PayloadByNumber(ctx, req)
	if err != nil {
		if errors.Is(err, ethereum.NotFound) {
			log.Warn("peer requested unknown block by number")
		} else {
			log.Error("failed to retrieve payload to serve to peer on P2P", "err", err)
		}
		return
	}
	log = log.New("payload", payload.ID())

	// We set write deadline, if available, to safely write without blocking on a throttling peer connection
	_ = stream.SetWriteDeadline(time.Now().Add(serverWriteChunkTimeout))

	var buf bytes.Buffer
	if _, err := payload.MarshalSSZ(&buf); err != nil {
		log.Error("failed to encode payload for sync response", "err", err)
		return
	}
	length := uint32(buf.Len())
	if err := binary.Write(stream, binary.LittleEndian, length); err != nil {
		log.Warn("failed to write response-length to sync response", "err", err, "size", length)
		return
	}
	if _, err := io.Copy(stream, &buf); err != nil {
		log.Warn("failed to write payload to sync response", "err", err, "size", length)
		return
	}
	log.Debug("successfully served sync response", "size", length)
}
