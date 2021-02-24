package privval

import (
	"fmt"
	"io"
	"log"
	"net"

	"github.com/BlockscapeNetwork/signctrl/config"
	"github.com/BlockscapeNetwork/signctrl/connection"
	"github.com/BlockscapeNetwork/signctrl/types"
	tm_protoio "github.com/tendermint/tendermint/libs/protoio"
	tm_privval "github.com/tendermint/tendermint/privval"
	tm_privvalproto "github.com/tendermint/tendermint/proto/tendermint/privval"
)

const (
	// KeyFile is Tendermint's default file name for the private validator's keys.
	KeyFile = "priv_validator_key.json"

	// StateFile is Tendermint's default file name for the private validator's state.
	StateFile = "priv_validator_state.json"

	// maxRemoteSignerMsgSize determines the maximum size in bytes for the delimited
	// reader.
	maxRemoteSignerMsgSize = 1024 * 10
)

// SCFilePV must implement the SignCtrled interface.
var _ types.SignCtrled = new(SCFilePV)

// SCFilePV is a wrapper for tm_privval.FilePV.
// Implements the SignCtrled interface by embedding BaseSignCtrled.
// Implements the Service interface by embedding BaseService.
type SCFilePV struct {
	types.BaseService
	types.BaseSignCtrled

	CurrentHeight int64
	Logger        *log.Logger
	Config        *config.Config
	TMFilePV      *tm_privval.FilePV
	SecretConn    net.Conn
	TermCh        chan struct{}
}

// KeyFilePath returns the absolute path to the priv_validator_key.json file.
func KeyFilePath(cfgDir string) string {
	return cfgDir + "/" + KeyFile
}

// StateFilePath returns the absolute path to the priv_validator_state.json file.
func StateFilePath(cfgDir string) string {
	return cfgDir + "/" + StateFile
}

// NewSCFilePV creates a new instance of SCFilePV.
func NewSCFilePV(logger *log.Logger, cfg *config.Config, tmpv *tm_privval.FilePV) *SCFilePV {
	pv := &SCFilePV{
		Logger:        logger,
		CurrentHeight: 1, // Start on genesis height
		Config:        cfg,
		TMFilePV:      tmpv,
		TermCh:        make(chan struct{}),
	}
	pv.BaseService = *types.NewBaseService(
		logger,
		"SignCTRL",
		pv,
	)
	pv.BaseSignCtrled = *types.NewBaseSignCtrled(
		logger,
		pv.Config.Init.Threshold,
		pv.Config.Init.Rank,
		pv,
	)

	return pv
}

// run runs the main loop of SignCTRL. It handles incoming messages from the validator.
// In order to stop the goroutine, Stop() should only be called outside of run(). The
// goroutine returns on its own once SignCTRL is forced to shut down.
func (pv *SCFilePV) run() {
	r := tm_protoio.NewDelimitedReader(pv.SecretConn, maxRemoteSignerMsgSize)
	w := tm_protoio.NewDelimitedWriter(pv.SecretConn)

	for {
		var msg tm_privvalproto.Message
		if _, err := r.ReadMsg(&msg); err != nil {
			if err == io.EOF {
				// Prevent the logs from being spammed with EOF errors
				continue
			}
			pv.Logger.Printf("[ERR] signctrl: couldn't read message: %v\n", err)
		}

		resp, err := HandleRequest(&msg, pv)
		if _, err := w.WriteMsg(resp); err != nil {
			pv.Logger.Printf("[ERR] signctrl: couldn't write message: %v\n", err)
		}
		if err != nil {
			pv.Logger.Printf("[ERR] signctrl: couldn't handle request: %v\n", err)
			if err == types.ErrMustShutdown {
				pv.Logger.Printf("[DEBUG] signctrl: Terminating run() goroutine")
				r.Close()
				w.Close()
				pv.Stop()
				return
			}
		}
	}
}

// OnStart starts the main loop of the SignCtrled PrivValidator.
// Implements the Service interface.
func (pv *SCFilePV) OnStart() (err error) {
	pv.Logger.Printf("[INFO] signctrl: Starting SignCTRL... (rank: %v)", pv.GetRank())

	// Load the connection key from the config directory.
	connKey, err := connection.LoadConnKey(config.Dir())
	if err != nil {
		return fmt.Errorf("Couldn't load conn.key: %v", err)
	}

	// Dial the validator.
	pv.SecretConn, err = connection.RetrySecretDialTCP(
		pv.Config.Init.ValidatorListenAddress,
		connKey,
		pv.Logger,
	)
	if err != nil {
		return fmt.Errorf("Couldn't dial validator: %v", err)
	}

	// Run the main loop.
	go pv.run()

	return nil
}

// OnStop terminates the main loop of the SignCtrled PrivValidator.
// Implements the Service interface.
func (pv *SCFilePV) OnStop() {
	pv.Logger.Printf("[INFO] signctrl: Stopping SignCTRL... (rank: %v)", pv.GetRank())
	pv.SecretConn.Close()   // Close the encrypted connection to the validator
	pv.TermCh <- struct{}{} // Signal termination
}
