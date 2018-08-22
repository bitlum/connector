package bitcoind

import (
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/bitlum/connector/connectors/daemons/bitcoind/btcjson"
	"github.com/bitlum/connector/connectors/daemons/bitcoind/rpcclient"
	"github.com/bitlum/connector/metrics/crypto"
	"github.com/btcsuite/btclog"
	"github.com/go-errors/errors"
	"github.com/shopspring/decimal"
	"github.com/bitlum/connector/connectors"
	"github.com/bitlum/connector/metrics"
	"github.com/AndrewSamokhvalov/go-spew/spew"
)

var (
	// allAccounts denotes that request should aggregate response for all
	// accounts available.
	allAccounts = "*"

	// defaultAccount denotes default account of wallet.
	defaultAccount = ""
)

const (
	MethodStart               = "Start"
	MethodAccountAddress      = "AccountAddress"
	MethodCreateAddress       = "CreateAddress"
	MethodPendingTransactions = "PendingTransactions"
	MethodGenerateTransaction = "GenerateTransaction"
	MethodSendTransaction     = "SendTransaction"
	MethodPendingBalance      = "PendingBalance"
	MethodSync                = "Sync"
	MethodValidate            = "Validate"
)

type DaemonConfig struct {
	Name       string
	ServerHost string
	ServerPort int
	User       string
	Password   string
}

// Config is a bitcoind config.
type Config struct {
	// Net blockchain network this connector should operate with.
	Net string

	// MinConfirmations is a minimum number of confirmations which is needed
	// to treat transaction as confirmed.
	MinConfirmations int

	// SyncLoopDelay is how much processing loop should sleep before
	// trying to update the information about
	SyncLoopDelay int

	// LastSyncedBlockHash is the hash of block which were proceeded last.
	// In this field is specified than, hash will be initialized form it,
	// rather than from database.
	LastSyncedBlockHash string

	// DaemonCfg holds the information about how to connect to the daemon
	// which interact with the payment system network.
	DaemonCfg *DaemonConfig

	// Asset is an asset with which this connector is working.
	Asset connectors.Asset

	// FeePerUnit fee per unit, where in bitcoin and litecoin unit is weight,
	// because of the weight, in dash it is byte.
	// TODO(andrew.shvv) Create subsystem to return current fee per unit
	FeePerUnit int

	Logger btclog.Logger

	// Metric is an metrics backend which is used for tracking the metrics of
	// connector.
	Metrics crypto.MetricsBackend

	// PaymentStorage is an external storage for payments, it is used by
	// connector to save payment as well as update its state.
	PaymentStore connectors.PaymentsStore

	// StateStorage is used to keep data which is needed for connector to
	// properly synchronise and track transactions.
	StateStorage connectors.StateStorage
}

func (c *Config) validate() error {
	if c.Net == "" {
		return errors.New("net should be specified")
	}

	if c.MinConfirmations <= 0 {
		return errors.New("min confirmations shouldn't be less or equal " +
			" zero")
	}

	if c.DaemonCfg == nil {
		return errors.New("daemon config should be specified")
	}

	if c.Logger == nil {
		return errors.New("logger should be specified")
	}

	if c.SyncLoopDelay == 0 {
		c.SyncLoopDelay = 5
	}

	if c.Asset == "" {
		return errors.New("asset should be specified")
	}

	if c.FeePerUnit == 0 {
		return errors.New("fee per unit should be specified")
	}

	if c.Metrics == nil {
		return errors.New("metrics backend should be specified")
	}

	if c.PaymentStore == nil {
		return errors.New("payment store should be specified")
	}

	return nil
}

// Connector implements connectors.BlockchainConnector interface for bitcoind
// client.
type Connector struct {
	started  int32
	shutdown int32
	wg       sync.WaitGroup
	quit     chan struct{}

	cfg    *Config
	client *ExtendedRPCClient

	// pending is a map of blockhain pending payments,
	// which hasn't been confirmed from connector point of view.
	// TODO(andrew.shvv) Remove because now we could use storage directly
	pending map[string][]*connectors.Payment

	lastSyncedBlockHash *chainhash.Hash
	netParams           *chaincfg.Params
	log                 *connectors.NamedLogger

	coinSelectMtx sync.Mutex

	// unspent is used to store btc uxto set locally, in order to craft
	// transactions faster.
	unspent map[string]btcjson.ListUnspentResult

	// unspentSyncMtx is used to lock the utxo local map during is
	// usage/population.
	unspentSyncMtx sync.Mutex
}

// A compile time check to ensure Connector implements the BlockchainConnector
// interface.
var _ connectors.BlockchainConnector = (*Connector)(nil)

func NewConnector(cfg *Config) (*Connector, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &Connector{
		cfg:  cfg,
		quit: make(chan struct{}),
		log: &connectors.NamedLogger{
			Name:   string(cfg.Asset),
			Logger: cfg.Logger,
		},
	}, nil
}

func (c *Connector) Start() error {
	if !atomic.CompareAndSwapInt32(&c.started, 0, 1) {
		c.log.Warn("client already started")
		return nil
	}

	m := crypto.NewMetric(c.cfg.DaemonCfg.Name, string(c.cfg.Asset),
		MethodStart, c.cfg.Metrics)
	defer m.Finish()

	host := fmt.Sprintf("%v:%v", c.cfg.DaemonCfg.ServerHost,
		c.cfg.DaemonCfg.ServerPort)
	cfg := &rpcclient.ConnConfig{
		Host:         host,
		User:         c.cfg.DaemonCfg.User,
		Pass:         c.cfg.DaemonCfg.Password,
		DisableTLS:   true, // TODO(andrew.shvv) switch on production
		HTTPPostMode: true,
	}

	// Create RPC client in order to talk with cryptocurrency daemon.
	c.log.Info("Creating RPC client...")
	client, err := rpcclient.New(cfg, nil)
	if err != nil {
		m.AddError(metrics.HighSeverity)
		return errors.Errorf("unable to create RPC client: %v", err)
	}
	c.client = &ExtendedRPCClient{
		Client: client,
	}

	var chain string
	if c.cfg.Asset == connectors.DASH {
		// Dash blockchain info response is different from standard bitcoin
		// blockchain info.
		resp, err := c.client.GetDashBlockChainInfo()
		if err != nil {
			m.AddError(metrics.HighSeverity)
			return errors.Errorf("unable to get type of network: %v", err)
		}
		chain = resp.Chain
	} else {
		resp, err := c.client.GetBlockChainInfo()
		if err != nil {
			m.AddError(metrics.HighSeverity)
			return errors.Errorf("unable to get type of network: %v", err)
		}
		chain = resp.Chain
	}

	if !isProperNet(c.cfg.Net, chain) {
		return errors.Errorf("networks are different, desired: %v, "+
			"actual: %v", c.cfg.Net, chain)
	}

	c.log.Infof("Init connector working with '%v' net", c.cfg.Net)

	c.netParams, err = getParams(c.cfg.Asset, chain)
	if err != nil {
		m.AddError(metrics.HighSeverity)
		return errors.Errorf("failed to get net params: %v", err)
	}

	// Initialize cache with the last synced block hash.
	c.log.Info("Getting last synced block hash...")
	if c.cfg.LastSyncedBlockHash != "" {
		c.lastSyncedBlockHash, err = chainhash.NewHashFromStr(c.cfg.LastSyncedBlockHash)
		if err != nil {
			m.AddError(metrics.HighSeverity)
			return errors.Errorf("unable to decode hash: %v", err)
		}
	} else {
		c.lastSyncedBlockHash, err = c.fetchLastSyncedBlockHash()
		if err != nil {
			m.AddError(metrics.HighSeverity)
			return errors.Errorf("unable to fetch last block synced "+
				"hash: %v", err)
		}
	}

	c.log.Infof("Last synced block hash(%v)", c.lastSyncedBlockHash)

	defaultAddress, err := c.fetchDefaultAddress()
	if err != nil {
		m.AddError(metrics.HighSeverity)
		return errors.Errorf("unable to fetch default address: %v", err)
	}
	c.log.Infof("Default address %v", defaultAddress)

	c.wg.Add(1)
	go func() {
		defer func() {
			c.log.Info("Quit syncing blocks goroutine")
			c.wg.Done()
		}()

		c.log.Info("Starting syncing goroutine...")

		syncDelay := time.Second * time.Duration(c.cfg.SyncLoopDelay)
		for {
			select {
			case <-time.After(syncDelay):
				if err := c.sync(); err != nil {
					c.log.Error(err)
					continue
				}
			case <-c.quit:
				return
			}

		}
	}()

	c.wg.Add(1)
	go func() {
		defer func() {
			c.log.Info("Quit syncing unspent goroutine")
			c.wg.Done()
		}()

		for {

			if err := c.syncUnspent(); err != nil {
				m.AddError(metrics.MiddleSeverity)
				c.log.Errorf("unable to main sync unspent: %v", err)
			}

			select {
			case <-time.After(time.Minute):
			case <-c.quit:
				return
			}
		}
	}()

	return err
}

func (c *Connector) Stop(reason string) {
	if !atomic.CompareAndSwapInt32(&c.shutdown, 0, 1) {
		c.log.Warn("client already shutdown")
		return
	}

	c.log.Infof("client shutting down (reason: %v)...", reason)
	close(c.quit)

	c.wg.Wait()

	c.log.Info("client shutdown")
}

func (c *Connector) WaitShutDown() <-chan struct{} {
	return c.quit
}

// AccountAddress return the deposit address of account.
//
// NOTE: Part of the connectors.BlockchainConnector interface.
func (c *Connector) AccountAddress(accountAlias string) (string, error) {
	m := crypto.NewMetric(c.cfg.DaemonCfg.Name, string(c.cfg.Asset),
		MethodAccountAddress, c.cfg.Metrics)
	defer m.Finish()

	addresses, err := c.client.GetAddressesByAccount(aliasToAccount(accountAlias))
	if err != nil {
		m.AddError(metrics.MiddleSeverity)
		return "", err
	}

	if len(addresses) == 0 {
		return "", nil
	}

	address := addresses[0].String()
	return address, nil
}

// CreateAddress is used to create deposit address.
//
// NOTE: Part of the connectors.BlockchainConnector interface.
func (c *Connector) CreateAddress(accountAlias string) (string, error) {
	m := crypto.NewMetric(c.cfg.DaemonCfg.Name, string(c.cfg.Asset),
		MethodCreateAddress, c.cfg.Metrics)
	defer m.Finish()

	address, err := c.client.GetNewAddress(aliasToAccount(accountAlias))
	if err != nil {
		m.AddError(metrics.HighSeverity)
		return "", err
	}

	return address.String(), nil
}

// PendingTransactions return the transactions which has confirmation
// number lower the required by payment system.
//
// NOTE: Part of the connectors.BlockchainConnector interface.
func (c *Connector) PendingTransactions(account string) (
	[]*connectors.Payment, error) {

	m := crypto.NewMetric(c.cfg.DaemonCfg.Name, string(c.cfg.Asset),
		MethodPendingTransactions, c.cfg.Metrics)
	defer m.Finish()

	// TODO(andrew.shvv) Use payment storage for getting pending transaction
	// and remove pending map.

	transactions := make([]*connectors.Payment, len(c.pending[account]))
	for i, tx := range c.pending[account] {
		transactions[i] = tx
	}

	return transactions, nil
}

// CreatePayment generates the payment, but not sends it,
// instead returns the payment id and waits for it to be approved.
//
// NOTE: Part of the connectors.BlockchainConnector interface.
func (c *Connector) CreatePayment(address string, amount string) (*connectors.Payment,
	error) {
	m := crypto.NewMetric(c.cfg.DaemonCfg.Name, string(c.cfg.Asset),
		MethodGenerateTransaction, c.cfg.Metrics)
	defer m.Finish()

	err := validateAddress(c.cfg.Asset, address, c.netParams.Name)
	if err != nil {
		m.AddError(metrics.LowSeverity)
		return nil, errors.Errorf("invalid address: %v", err)
	}

	decodedAddress, err := btcutil.DecodeAddress(address, c.netParams)
	if err != nil {
		m.AddError(metrics.LowSeverity)

		return nil, errors.Errorf("unable to decode address: %v", err)
	}

	amt, err := decimal.NewFromString(amount)
	if err != nil {
		m.AddError(metrics.LowSeverity)
		return nil, errors.Errorf("unable to decode amount: %v", err)
	}

	tx, fee, err := c.craftTransaction(uint64(c.cfg.FeePerUnit), decAmount2Sat(amt), decodedAddress)
	if err != nil {
		m.AddError(metrics.HighSeverity)
		return nil, errors.Errorf("unable to generate new transaction: %v", err)
	}

	signedTx, isSigned, err := c.client.SignRawTransaction(tx)
	if err != nil {
		m.AddError(metrics.HighSeverity)
		return nil, errors.Errorf("unable to sign generated transaction: %v", err)
	}

	if !isSigned {
		m.AddError(metrics.HighSeverity)
		return nil, errors.Errorf("unable to sign all generated transaction"+
			" inputs: %v", err)
	}

	var rawTx bytes.Buffer
	if err := signedTx.Serialize(&rawTx); err != nil {
		m.AddError(metrics.HighSeverity)
		return nil, errors.Errorf("unable serialize signed tx: %v", err)
	}

	txID := signedTx.TxHash().String()

	payment := &connectors.Payment{
		PaymentID: generatePaymentID(txID, address, connectors.Outgoing),
		UpdatedAt: time.Now().Unix(),
		Status:    connectors.Waiting,
		Direction: connectors.Outgoing,
		Receipt:   address,
		Asset:     connectors.Asset(c.cfg.Asset),
		Media:     connectors.Blockchain,
		Amount:    amt,
		MediaFee:  sat2DecAmount(fee),
		MediaID:   txID,
		Detail: &connectors.GeneratedTxDetails{
			RawTx: rawTx.Bytes(),
			TxID:  txID,
		},
	}

	if err := c.cfg.PaymentStore.SavePayment(payment); err != nil {
		m.AddError(metrics.HighSeverity)
		return nil, errors.Errorf("unable add payment in store: %v", err)
	}

	return payment, nil
}

// SendPayment sends created previously payment to the
// blockchain network.
//
// NOTE: Part of the connectors.BlockchainConnector interface.
func (c *Connector) SendPayment(paymentID string) (*connectors.Payment, error) {
	m := crypto.NewMetric(c.cfg.DaemonCfg.Name, string(c.cfg.Asset),
		MethodSendTransaction, c.cfg.Metrics)
	defer m.Finish()

	payment, err := c.cfg.PaymentStore.PaymentByID(paymentID)
	if err != nil {
		m.AddError(metrics.HighSeverity)
		return nil, errors.Errorf("unable find payment(%v): %v", paymentID,
			err)
	}

	details, ok := payment.Detail.(*connectors.GeneratedTxDetails)
	if !ok {
		return nil, errors.Errorf("unable get details for payment(%v)",
			paymentID)
	}

	wireTx := new(wire.MsgTx)
	r := bytes.NewBuffer(details.RawTx)

	if err := wireTx.Deserialize(r); err != nil {
		m.AddError(metrics.HighSeverity)
		return nil, errors.Errorf("unable to deserialize raw tx: %v", err)
	}

	_, err = c.client.SendRawTransaction(wireTx, true)
	if err != nil {
		payment.Status = connectors.Failed
		payment.UpdatedAt = time.Now().Unix()

		err = c.cfg.PaymentStore.SavePayment(payment)
		if err != nil {
			m.AddError(metrics.HighSeverity)
			c.log.Errorf("unable update payment(%v) status to fail: %v",
				paymentID, err)
		}

		m.AddError(metrics.HighSeverity)
		return nil, errors.Errorf("unable to send transaction: %v", err)
	}

	payment.Status = connectors.Pending
	payment.UpdatedAt = time.Now().Unix()

	err = c.cfg.PaymentStore.SavePayment(payment)
	if err != nil {
		m.AddError(metrics.HighSeverity)
		c.log.Errorf("unable update payment(%v) status to pending: %v",
			paymentID, err)
	}

	return payment, nil
}

// ConfirmedBalance returns number of funds available under control of
// connector.
//
// NOTE: Part of the connectors.Connector interface.
func (c *Connector) ConfirmedBalance(accountAlias string) (decimal.Decimal, error) {
	account := aliasToAccount(accountAlias)
	balance, err := c.client.GetBalanceMinConf(account, c.cfg.MinConfirmations)
	if err != nil {
		return decimal.Zero, err
	}

	return decimal.NewFromFloat(balance.ToBTC()).Round(8), nil
}

// PendingBalance return the amount of funds waiting ro be confirmed.
//
// NOTE: Part of the connectors.BlockchainConnector interface.
func (c *Connector) PendingBalance(accountAlias string) (decimal.Decimal, error) {
	m := crypto.NewMetric(c.cfg.DaemonCfg.Name, string(c.cfg.Asset),
		MethodPendingBalance, c.cfg.Metrics)
	defer m.Finish()

	// TODO(andrew.shvv) Use storage for getting pending transaction and
	// calculation of pending balance.

	var amount decimal.Decimal
	if accountAlias == "all" {
		for _, pendingPayments := range c.pending {
			for _, payment := range pendingPayments {
				amount = amount.Add(payment.Amount)
			}
		}
	} else {
		for _, payment := range c.pending[accountAlias] {
			amount = amount.Add(payment.Amount)
		}
	}

	return amount.Round(8), nil
}

// syncUnconfirmed updates status of incoming payments, from waiting to
// pending, as well as blockchain detail and number of transaction needed for
// it to be confirmed.
func (c *Connector) syncUnconfirmed() error {
	// Return set of non-confirmed from our point of view incoming
	// transactions.
	txs, err := c.client.ListUnspentMinMax(0, int(c.cfg.MinConfirmations-1))
	if err != nil {
		return err
	}

	c.pending = make(map[string][]*connectors.Payment)
	for _, tx := range txs {
		payment := &connectors.Payment{
			UpdatedAt: time.Now().Unix(),
			Status:    connectors.Pending,
			Receipt:   tx.Address,
			Asset:     c.cfg.Asset,
			Account:   accountToAlias(tx.Account),
			Media:     connectors.Blockchain,
			Amount:    decimal.NewFromFloat(tx.Amount),
			MediaFee:  decimal.Zero,
			MediaID:   tx.TxID,
			Detail: &connectors.BlockchainPendingDetails{
				Confirmations:     tx.Confirmations,
				ConfirmationsLeft: int64(c.cfg.MinConfirmations) - tx.Confirmations,
			},
		}

		if tx.Account == defaultAccount {
			payment.Direction = connectors.Internal
			payment.MediaFee = decimal.Zero
			payment.PaymentID = generatePaymentID(tx.TxID, tx.Address,
				connectors.Internal)
		} else {
			payment.Direction = connectors.Incoming
			payment.MediaFee = decimal.Zero
			payment.PaymentID = generatePaymentID(tx.TxID, tx.Address,
				connectors.Incoming)
		}

		// TODO(andrew.shvv) Remove because now we could use storage directly
		// for pending balance and pending transaction.
		c.pending[tx.Account] = append(c.pending[tx.Account], payment)

		if err := c.cfg.PaymentStore.SavePayment(payment); err != nil {
			return errors.Errorf("unable to save payment(%v): %v",
				payment.PaymentID, err)
		}

		c.log.Infof("Pending transaction(%v),"+
			"confirmations left(%v), account(%v), amount(%v)", tx.TxID,
			int64(c.cfg.MinConfirmations)-tx.Confirmations,
			accountToAlias(tx.Account), tx.Amount)
	}

	return nil
}

// findForkBlock is used to find block on which fork has happened,
// at return it, so that syncing could continue.
func (c *Connector) findForkBlock(orphanBlock *btcjson.GetBlockVerboseResult) (
	*btcjson.GetBlockVerboseResult, error) {
	for orphanBlock.Confirmations == -1 {
		prevHash, err := chainhash.NewHashFromStr(orphanBlock.PreviousHash)
		if err != nil {
			return nil, errors.Errorf("unable to decode hash of prev "+
				"orphan block: %v", err)
		}

		orphanBlock, err = c.client.GetBlockVerbose(prevHash)
		if err != nil {
			return nil, errors.Errorf("unable to prev last sync block "+
				"from daemon: %v", err)

		}
	}

	return orphanBlock, nil
}

// proceedNextBlock process new blocks and updates payment status that
// transaction reached the minimum confirmation threshold.
func (c *Connector) proceedNextBlock() error {
	lastSyncedBlock, err := c.client.GetBlockVerbose(c.lastSyncedBlockHash)
	if err != nil {
		return errors.Errorf("unable to get last sync block "+
			"from daemon: %v", err)
	}

	// If bitcoind returns negative confirmation number it means that
	// blockchain re-organization happened and we should handle it properly by
	// moving backwards.
	if lastSyncedBlock.Confirmations < 0 {
		c.log.Info("Chain re-organisation has been found, handle it...")

		forkBlock, err := c.findForkBlock(lastSyncedBlock)
		if err != nil {
			return errors.Errorf("unable to handle "+
				"re-organizations: %v", err)
		}

		c.log.Infof("Fork have been detected on block("+
			"%v) using it as last synced block", forkBlock.Hash)
		lastSyncedBlock = forkBlock
	}

	for {
		select {
		case <-c.quit:
			return nil
		default:
		}

		// We should check next block only if there is minimum amount of
		// confirmation above it.
		if lastSyncedBlock.Confirmations < int64(c.cfg.MinConfirmations)+1 {
			return nil
		}

		// This check is a bit redundant, but we should be ensured that
		// next hash exists, otherwise the last synced hash will be overwritten
		// with zero hash.
		if lastSyncedBlock.NextHash == "" {
			c.log.Errorf("unable to continue processing block(%v):"+
				"next hash empty", lastSyncedBlock.Hash)
			return nil
		}

		nextHash, err := chainhash.NewHashFromStr(lastSyncedBlock.NextHash)
		if err != nil {
			return err
		}

		proceededBlock, err := c.client.GetBlockVerbose(nextHash)
		if err != nil {
			return err
		}

		for _, txHashStr := range proceededBlock.Tx {
			txHash, err := chainhash.NewHashFromStr(txHashStr)
			if err != nil {
				c.log.Errorf("unable to decode tx hash(%v)", txHashStr)
				continue
			}

			// Get transaction and if this transaction not correspond to non
			// of our account the error will be returned, in the case skip
			// this transaction.
			tx, err := c.client.GetTransaction(txHash)
			if err != nil {
				continue
			}

			if len(tx.Details) == 0 {
				c.log.Errorf("unable to sync tx(%v), there is "+
					"no details", tx.TxID)
				continue
			}

			for _, detail := range tx.Details {
				payment := &connectors.Payment{
					UpdatedAt: time.Now().Unix(),
					Status:    connectors.Completed,
					Receipt:   detail.Address,
					Asset:     c.cfg.Asset,
					Account:   accountToAlias(detail.Account),
					Media:     connectors.Blockchain,
					MediaID:   tx.TxID,
				}

				if detail.Category == "receive" &&
					detail.Account == defaultAccount {

					payment.MediaFee = decimal.Zero
					payment.Direction = connectors.Internal
					payment.Amount = decimal.NewFromFloat(detail.Amount)
					payment.PaymentID = generatePaymentID(tx.TxID,
						detail.Address, connectors.Internal)

				} else if detail.Category == "receive" {
					payment.Direction = connectors.Incoming
					payment.MediaFee = decimal.Zero
					payment.Amount = decimal.NewFromFloat(detail.Amount)
					payment.PaymentID = generatePaymentID(tx.TxID,
						detail.Address, connectors.Incoming)

				} else if detail.Category == "send" {
					payment.PaymentID = generatePaymentID(tx.TxID,
						detail.Address, connectors.Outgoing)

					_, err := c.cfg.PaymentStore.PaymentByID(payment.PaymentID)
					if err != nil {
						// If payment is not found in the storage that means
						// that this is the "change". Such check only works if
						// payment id consist of address and txid.
						continue
					}

					payment.Amount = decimal.NewFromFloat(detail.Amount).Abs()
					payment.MediaFee = decimal.NewFromFloat(tx.Fee).Abs()
					payment.Direction = connectors.Outgoing
				}

				c.log.Warn(payment.PaymentID, spew.Sdump(detail))
				if err := c.cfg.PaymentStore.SavePayment(payment); err != nil {
					return errors.Errorf("unable to save payment(%v): %v",
						payment.PaymentID, err)
				}
			}
		}

		// ...
		err = c.cfg.StateStorage.PutLastSyncedHash(nextHash.CloneBytes())
		if err != nil {
			return errors.Errorf("unable to put block hash in db: %v", err)
		}

		c.lastSyncedBlockHash = nextHash
		lastSyncedBlock = proceededBlock

		// After transaction has been consumed by other subsystem
		// overwrite cache.
		c.log.Infof("Process block hash(%v)", proceededBlock.Hash)
	}
}

// fetchLastSyncedBlockHash returns hash of block which were handled in previous
// cycle of processing.
func (c *Connector) fetchLastSyncedBlockHash() (*chainhash.Hash, error) {
	c.log.Info("Restore hash from database...")
	data, _ := c.cfg.StateStorage.LastSyncedHash()
	if data != nil {
		lastHash, err := chainhash.NewHash(data)
		if err != nil {
			return nil, errors.Errorf("unable initialize hash: %v", err)
		}

		return lastHash, nil
	}

	c.log.Info("Unable to find block in db, fetching best block...")
	lastHash, err := c.client.GetBestBlockHash()
	if err != nil {
		return nil, errors.Errorf("unable to request last best block "+
			"hash: %v", err)
	}

	err = c.cfg.StateStorage.PutLastSyncedHash(lastHash.CloneBytes())
	if err != nil {
		return nil, errors.Errorf("unable to put block hash in db: %v", err)
	}

	return lastHash, nil
}

// fetchDefaultAddress...
func (c *Connector) fetchDefaultAddress() (string, error) {
	defaultAddress, err := c.AccountAddress("default")
	if err != nil {
		return "", errors.Errorf("unable to get default address: %v", err)
	}

	if defaultAddress == "" {
		c.log.Info("Unable to find default address in db, generating it...")
		defaultAddress, err = c.CreateAddress("default")
		if err != nil {
			return "", errors.Errorf("unable to generate default address: %v", err)
		}
	}

	return defaultAddress, nil
}

func (c *Connector) sync() error {
	m := crypto.NewMetric(c.cfg.DaemonCfg.Name, string(c.cfg.Asset),
		MethodSync, c.cfg.Metrics)
	defer m.Finish()

	if err := c.proceedNextBlock(); err != nil {
		m.AddError(metrics.MiddleSeverity)
		return errors.Errorf("unable to process blocks: %v", err)

	}

	// As far as pending transaction may occur at any time,
	// run it every cycle.
	if err := c.syncUnconfirmed(); err != nil {
		m.AddError(metrics.MiddleSeverity)
		return errors.Errorf("unable to sync unconfirmed txs: %v", err)
	}

	balance, err := c.ConfirmedBalance("all")
	if err != nil {
		m.AddError(metrics.MiddleSeverity)
		return errors.Errorf("unable to get available funds: %v", err)
	}

	c.log.Infof("Asset(%v), media(blockchain), available funds(%v)",
		c.cfg.Asset, balance.Round(8).String())

	f, _ := balance.Float64()
	m.CurrentFunds(f)

	return nil
}

// ValidateAddress takes the blockchain address and ensure its validity.
func (c *Connector) ValidateAddress(address string) error {
	m := crypto.NewMetric(c.cfg.DaemonCfg.Name, string(c.cfg.Asset),
		MethodValidate, c.cfg.Metrics)
	defer m.Finish()

	err := validateAddress(c.cfg.Asset, address, c.netParams.Name)
	if err != nil {
		m.AddError(metrics.LowSeverity)
		return errors.Errorf("invalid address: %v", err)
	}

	return nil
}

// EstimateFee estimate fee for the transaction with the given sending
// amount.
//
// NOTE: Fee depends on amount because of the number amount of inputs
// which has to be used to construct the transaction.
func (c *Connector) EstimateFee(amount string) (decimal.Decimal, error) {
	m := crypto.NewMetric(c.cfg.DaemonCfg.Name, string(c.cfg.Asset),
		MethodValidate, c.cfg.Metrics)
	defer m.Finish()

	// Fee estimation for simnet and testnet working unstable,
	// for that reason just return zero.
	if c.cfg.Net == "simnet" || c.cfg.Net == "testnet" {
		return decimal.Zero, nil
	}

	feeRate, err := c.getFeeRate()
	if err != nil {
		return decimal.Zero, err
	}

	// Estimate fee for the median transaction size of 225 bytes.
	// TODO(andrew.shvv) Use amount to construct actual transaction and
	// calculate its size.
	size := decimal.New(225, 0)
	satoshiFee := feeRate.Mul(size)
	return satoshiFee.Div(satoshiPerBitcoin), nil
}

func (c *Connector) getFeeRate() (decimal.Decimal, error) {
	switch c.cfg.Asset {
	case connectors.BCH, connectors.DASH:
		// Bitcoin Cash removed estimatesmartfee in 17.2 version of their client,
		// for that reason we need to have different behaviour for Bitcoin Cash
		// asset, and use original estimatefee method.
		res, err := c.client.EstimateFee(2)
		if err != nil {
			return decimal.Zero, err
		}

		feeRate := decimal.NewFromFloat(**res)
		if feeRate.LessThanOrEqual(decimal.Zero) {
			return decimal.Zero, errors.New("not enough data to make an estimation")
		}

		return feeRate, nil

	default:
		res, err := c.client.EstimateSmartFeeWithMode(2,
			btcjson.ConservativeEstimateMode)
		if err != nil {
			return decimal.Zero, err
		}

		if res.Errors != nil {
			return decimal.Zero, errors.New((*res.Errors)[0])
		}

		feeRate := decimal.NewFromFloat(*res.FeeRate)
		return feeRate, nil
	}
}