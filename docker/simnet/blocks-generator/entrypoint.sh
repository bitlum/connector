#!/usr/bin/env bash

RPC_USER="user"
RPC_PASSWORD="password"

BTC_RPC_HOST="bitcoin.simnet.primary"
BTC_RPC_PORT=8332
BTC_OPTS="\
--rpcconnect=$BTC_RPC_HOST --rpcport=$BTC_RPC_PORT \
--rpcuser=$RPC_USER --rpcpassword=$RPC_PASSWORD \
--regtest"

BCH_RPC_HOST="bitcoin-cash.simnet.primary"
BCH_RPC_PORT=9332
BCH_OPTS="\
--rpcconnect=$BCH_RPC_HOST --rpcport=$BCH_RPC_PORT \
--rpcuser=$RPC_USER --rpcpassword=$RPC_PASSWORD \
--regtest"

DASH_RPC_HOST="dash.simnet.primary"
DASH_RPC_PORT=10332
DASH_OPTS="\
--rpcconnect=$DASH_RPC_HOST --rpcport=$DASH_RPC_PORT \
--rpcuser=$RPC_USER --rpcpassword=$RPC_PASSWORD \
--regtest"

LTC_RPC_HOST="litecoin.simnet.primary"
LTC_RPC_PORT=12332
LTC_OPTS="\
--rpcconnect=$LTC_RPC_HOST --rpcport=$LTC_RPC_PORT \
--rpcuser=$RPC_USER --rpcpassword=$RPC_PASSWORD \
--regtest"

# We need to wait until all primary blockchain nodes are started.
echo "$(date '+%Y-%m-%d %H:%M:%S') Waiting for all primary blockchains nodes are started"
sleep 60

# Initial blocks generation.
echo "$(date '+%Y-%m-%d %H:%M:%S') Initial blocks generation"
bitcoin-cli $BTC_OPTS generate 400
bitcoin-cash-cli $BCH_OPTS generate 100
dash-cli $DASH_OPTS generate 100
litecoin-cli $LTC_OPTS generate 100

# Proper way to catch shutdown signal and stop infinite loop. In this
# solution we consider blockchains' cli runs are not long running process
# so we need to stop sleep only.
# Check http://veithen.github.io/2014/11/16/sigterm-propagation.html.
shutdown() {
    kill -s SIGTERM $!
    exit 0
}
trap shutdown SIGINT SIGTERM

# Periodically block generation.
while true
do
    echo "$(date '+%Y-%m-%d %H:%M:%S') Periodical blocks generation"
    bitcoin-cli $BTC_OPTS generate 1
    bitcoin-cash-cli $BCH_OPTS generate 1
    dash-cli $DASH_OPTS generate 1
    litecoin-cli $LTC_OPTS generate 1

    # Wait for next period.
    sleep $PERIOD &
    wait $!
done