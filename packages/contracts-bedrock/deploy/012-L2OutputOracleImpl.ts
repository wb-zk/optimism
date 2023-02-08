import { DeployFunction } from 'hardhat-deploy/dist/types'
import '@eth-optimism/hardhat-deploy-config'
import '@nomiclabs/hardhat-ethers'

import { assertContractVariable, deploy } from '../src/deploy-utils'

const deployFn: DeployFunction = async (hre) => {
  await deploy({
    hre,
    name: 'L2OutputOracle',
    args: [
      hre.deployConfig.l2BlockTime,
      0,
      0,
      hre.deployConfig.l2OutputOracleChallenger,
    ],
    postDeployAction: async (contract) => {
      await assertContractVariable(
        contract,
        'L2_BLOCK_TIME',
        hre.deployConfig.l2BlockTime
      )
      await assertContractVariable(
        contract,
        'CHALLENGER',
        hre.deployConfig.l2OutputOracleChallenger
      )
    },
  })
}

deployFn.tags = ['L2OutputOracleImpl', 'setup']

export default deployFn
