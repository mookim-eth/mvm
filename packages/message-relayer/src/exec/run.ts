import { Wallet, providers } from 'ethers'
import { MessageRelayerService } from '../service'
import { Bcfg } from '@metis.io/core-utils'
import { Logger, LoggerOptions } from '@eth-optimism/common-ts'
import * as Sentry from '@sentry/node'
import * as dotenv from 'dotenv'
import Config from 'bcfg'

dotenv.config()

const main = async () => {
  const config: Bcfg = new Config('message-relayer')
  config.load({
    env: true,
    argv: true,
  })

  const env = process.env

  const SENTRY_DSN = config.str('sentry-dsn', env.SENTRY_DSN)
  const USE_SENTRY = config.bool('use-sentry', env.USE_SENTRY === 'true')
  const ETH_NETWORK_NAME = config.str('eth-network-name', env.ETH_NETWORK_NAME)

  const loggerOptions: LoggerOptions = {
    name: 'Message_Relayer',
  }

  if (USE_SENTRY) {
    const sentryOptions = {
      release: `message-relayer@${process.env.npm_package_version}`,
      dsn: SENTRY_DSN,
      environment: ETH_NETWORK_NAME,
    }
    loggerOptions.sentryOptions = sentryOptions
    Sentry.init(sentryOptions)
  }

  const logger = new Logger(loggerOptions)

  const L2_NODE_WEB3_URL = config.str('l2-node-web3-url', env.L2_NODE_WEB3_URL)
  const L1_NODE_WEB3_URL = config.str('l1-node-web3-url', env.L1_NODE_WEB3_URL)
  const L2_NODE_CHAIN_ID = config.uint(
    'l2-node-chain-id',
    parseInt(env.CHAIN_ID, 10) || 0
  )
  const ADDRESS_MANAGER_ADDRESS = config.str(
    'address-manager-address',
    env.ADDRESS_MANAGER_ADDRESS
  )
  const L1_WALLET_KEY = config.str('l1-wallet-key', env.L1_WALLET_KEY)
  const MNEMONIC = config.str('mnemonic', env.MNEMONIC)
  const HD_PATH = config.str('hd-path', env.HD_PATH)
  const RELAY_GAS_LIMIT = config.uint(
    'relay-gas-limit',
    parseInt(env.RELAY_GAS_LIMIT, 10) || 4000000
  )
  const POLLING_INTERVAL = config.uint(
    'polling-interval',
    parseInt(env.POLLING_INTERVAL, 10) || 5000
  )
  const GET_LOGS_INTERVAL = config.uint(
    'get-logs-interval',
    parseInt(env.GET_LOGS_INTERVAL, 10) || 2000
  )
  const L2_BLOCK_OFFSET = config.uint(
    'l2-start-offset',
    parseInt(env.L2_BLOCK_OFFSET, 10) || 1
  )
  const L1_START_OFFSET = config.uint(
    'l1-start-offset',
    parseInt(env.L1_BLOCK_OFFSET, 10) || 1
  )
  const FROM_L2_TRANSACTION_INDEX = config.uint(
    'from-l2-transaction-index',
    parseInt(env.FROM_L2_TRANSACTION_INDEX, 10) || 0
  )

  if (!ADDRESS_MANAGER_ADDRESS) {
    throw new Error('Must pass ADDRESS_MANAGER_ADDRESS')
  }
  if (!L1_NODE_WEB3_URL) {
    throw new Error('Must pass L1_NODE_WEB3_URL')
  }
  if (!L2_NODE_WEB3_URL) {
    throw new Error('Must pass L2_NODE_WEB3_URL')
  }
  const USE_CHAIN_STORE = config.bool(
    'user-chain-store',
    env.USE_CHAIN_STORE === 'true'
  )
  const STORE_DB_URL: string = config.str('store-db-url', env.STORE_DB_URL)
  const RELAY_NUMBER: number = config.uint(
    'relay-number',
    parseInt(env.RELAY_NUMBER, 10) || 0
  )

  const l2Provider = new providers.StaticJsonRpcProvider(L2_NODE_WEB3_URL)
  const l1Provider = new providers.StaticJsonRpcProvider(L1_NODE_WEB3_URL)

  let wallet: Wallet
  if (L1_WALLET_KEY) {
    wallet = new Wallet(L1_WALLET_KEY, l1Provider)
  } else if (MNEMONIC) {
    wallet = Wallet.fromMnemonic(MNEMONIC, HD_PATH)
    wallet = wallet.connect(l1Provider)
  } else {
    throw new Error('Must pass one of L1_WALLET_KEY or MNEMONIC')
  }
  var chainId = L2_NODE_CHAIN_ID
  if (!chainId || chainId == 0) {
    chainId = await l2Provider.send('eth_chainId', [])
  }
  const service = new MessageRelayerService({
    l1RpcProvider: l1Provider,
    l2RpcProvider: l2Provider,
    l2ChainId: chainId,
    addressManagerAddress: ADDRESS_MANAGER_ADDRESS,
    l1Wallet: wallet,
    relayGasLimit: RELAY_GAS_LIMIT,
    fromL2TransactionIndex: FROM_L2_TRANSACTION_INDEX,
    pollingInterval: POLLING_INTERVAL,
    l2BlockOffset: L2_BLOCK_OFFSET,
    l1StartOffset: L1_START_OFFSET,
    getLogsInterval: GET_LOGS_INTERVAL,
    logger,
    useChainStore: USE_CHAIN_STORE,
    storeDbUrl: STORE_DB_URL,
    relayNumber: RELAY_NUMBER,
  })

  await service.start()
}
export default main
