// SPDX-License-Identifier: MIT
pragma solidity 0.8.15;

import { Initializable } from "@openzeppelin/contracts/proxy/utils/Initializable.sol";
import { Semver } from "../universal/Semver.sol";
import { Types } from "../libraries/Types.sol";

/**
 * @custom:proxied
 * @title L2OutputOracle
 * @notice The L2OutputOracle contains an array of L2 state outputs, where each output is a
 *         commitment to the state of the L2 chain. Other contracts like the OptimismPortal use
 *         these outputs to verify information about the state of L2.
 */
contract L2OutputOracle is Initializable, Semver {
    /**
     * @notice The time between L2 blocks in seconds. Once set, this value MUST NOT be modified.
     */
    uint256 public immutable L2_BLOCK_TIME;

    /**
     * @notice The address of the challenger. Can be updated via upgrade.
     */
    address public immutable CHALLENGER;

    /**
     * @notice The number of the first L2 block recorded in this contract.
     */
    uint256 public startingBlockNumber;

    /**
     * @notice The timestamp of the first L2 block recorded in this contract.
     */
    uint256 public startingTimestamp;

    /**
     * @notice Highest L2 block number that has been proposed.
     */
    uint256 public highestL2BlockNumber;

    /**
     * @notice Array of L2 output proposals.
     */
    mapping (uint256 => Types.OutputProposal) internal l2Outputs;

    /**
     * @notice Emitted when an output is proposed.
     *
     * @param outputRoot    The output root.
     * @param l2BlockNumber The L2 block number of the output root.
     * @param l1Timestamp   The L1 timestamp when proposed.
     */
    event OutputProposed(
        bytes32 indexed outputRoot,
        uint256 indexed l2BlockNumber,
        uint256 l1Timestamp
    );

    /**
     * @notice Emitted when outputs are deleted.
     *
     * @param l2BlockNumber Block number of the proposal that was deleted.
     */
    event OutputDeleted(uint256 indexed l2BlockNumber);

    /**
     * @custom:semver 1.0.0
     *
     * @param _l2BlockTime         The time per L2 block, in seconds.
     * @param _startingBlockNumber The number of the first L2 block.
     * @param _startingTimestamp   The timestamp of the first L2 block.
     * @param _challenger          The address of the challenger.
     */
    constructor(
        uint256 _l2BlockTime,
        uint256 _startingBlockNumber,
        uint256 _startingTimestamp,
        address _challenger
    ) Semver(1, 0, 0) {
        L2_BLOCK_TIME = _l2BlockTime;
        CHALLENGER = _challenger;

        initialize(_startingBlockNumber, _startingTimestamp);
    }

    /**
     * @notice Initializer.
     *
     * @param _startingBlockNumber Block number for the first recoded L2 block.
     * @param _startingTimestamp   Timestamp for the first recoded L2 block.
     */
    function initialize(uint256 _startingBlockNumber, uint256 _startingTimestamp)
        public
        initializer
    {
        require(
            _startingTimestamp <= block.timestamp,
            "L2OutputOracle: starting L2 timestamp must be less than current time"
        );

        startingTimestamp = _startingTimestamp;
        startingBlockNumber = _startingBlockNumber;
    }

    /**
     * @notice Deletes an output proposal based on the given L2 block number.
     *
     * @param _l2BlockNumber The L2 block number of the output to delete.
     */
    // solhint-disable-next-line ordering
    function deleteL2Output(uint256 _l2BlockNumber) external {
        require(
            msg.sender == CHALLENGER,
            "L2OutputOracle: only the challenger address can delete outputs"
        );

        l2Outputs[_l2BlockNumber] = Types.OutputProposal({
            outputRoot: bytes32(0),
            timestamp: 0,
            l2BlockNumber: 0
        });

        emit OutputDeleted(_l2BlockNumber);
    }

    /**
     * @notice Accepts an outputRoot and the timestamp of the corresponding L2 block. The timestamp
     *         must be equal to the current value returned by `nextTimestamp()` in order to be
     *         accepted.
     *
     * @param _outputRoot    The L2 output of the checkpoint block.
     * @param _l2BlockNumber The L2 block number that resulted in _outputRoot.
     * @param _l1BlockHash   A block hash which must be included in the current chain.
     * @param _l1BlockNumber The block number with the specified block hash.
     */
    function proposeL2Output(
        bytes32 _outputRoot,
        uint256 _l2BlockNumber,
        bytes32 _l1BlockHash,
        uint256 _l1BlockNumber
    ) external payable {
        require(
            l2Outputs[_l2BlockNumber].timestamp == 0,
            "L2OutputOracle: output already proposed"
        );

        require(
            computeL2Timestamp(_l2BlockNumber) < block.timestamp,
            "L2OutputOracle: cannot propose L2 output in the future"
        );

        require(
            _outputRoot != bytes32(0),
            "L2OutputOracle: L2 output proposal cannot be the zero hash"
        );

        if (_l1BlockHash != bytes32(0)) {
            // This check allows the proposer to propose an output based on a given L1 block,
            // without fear that it will be reorged out.
            // It will also revert if the blockheight provided is more than 256 blocks behind the
            // chain tip (as the hash will return as zero). This does open the door to a griefing
            // attack in which the proposer's submission is censored until the block is no longer
            // retrievable, if the proposer is experiencing this attack it can simply leave out the
            // blockhash value, and delay submission until it is confident that the L1 block is
            // finalized.
            require(
                blockhash(_l1BlockNumber) == _l1BlockHash,
                "L2OutputOracle: block hash does not match the hash at the expected height"
            );
        }

        // Update the highest L2 block number if necessary.
        if (_l2BlockNumber > highestL2BlockNumber) {
            highestL2BlockNumber = _l2BlockNumber;
        }

        l2Outputs[_l2BlockNumber] =
            Types.OutputProposal({
                outputRoot: _outputRoot,
                timestamp: uint128(block.timestamp),
                l2BlockNumber: uint128(_l2BlockNumber)
            });

        emit OutputProposed(_outputRoot, _l2BlockNumber, block.timestamp);
    }

    /**
     * @notice Returns an output by index. Exists because Solidity's mapping access will return a
     *         tuple instead of a struct.
     * TODO: Still necessary with mappings?
     *
     * @param _l2BlockNumber L2 block number of the output to return.
     *
     * @return The output for the given block number.
     */
    function getL2Output(uint256 _l2BlockNumber)
        external
        view
        returns (Types.OutputProposal memory)
    {
        return l2Outputs[_l2BlockNumber];
    }

    /**
     * @notice Returns the L2 output proposal that checkpoints a given L2 block number. Uses an
     *         inefficient linear search, so this method is really only useful for off-chain access
     *         and is not meant to be feasible for on-chain access.
     *
     * @param _l2BlockNumber L2 block number to find a checkpoint for.
     *
     * @return First checkpoint that commits to the given L2 block number.
     */
    function getL2OutputAfter(uint256 _l2BlockNumber)
        external
        view
        returns (Types.OutputProposal memory)
    {
        // Make sure we aren't exceeding the max.
        require(
            _l2BlockNumber <= highestL2BlockNumber,
            "L2OutputOracle: cannot get output for block number that has not been proposed"
        );

        uint256 l2BlockNumber = _l2BlockNumber;
        Types.OutputProposal memory proposal = l2Outputs[l2BlockNumber];
        while (proposal.timestamp == 0) {
            l2BlockNumber++;
            proposal = l2Outputs[l2BlockNumber];

            // Make sure we haven't exceeded the max yet.
            require(
                l2BlockNumber <= highestL2BlockNumber,
                "L2OutputOracle: cannot get output for block number that has not been proposed"
            );
        }

        return l2Outputs[l2BlockNumber];
    }

    /**
     * @notice Returns the L2 timestamp corresponding to a given L2 block number.
     *
     * @param _l2BlockNumber The L2 block number of the target block.
     *
     * @return L2 timestamp of the given block.
     */
    function computeL2Timestamp(uint256 _l2BlockNumber) public view returns (uint256) {
        return startingTimestamp + ((_l2BlockNumber - startingBlockNumber) * L2_BLOCK_TIME);
    }
}
