package tss

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	coskey "github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	sdk "github.com/cosmos/cosmos-sdk/types/bech32/legacybech32"
	"github.com/libp2p/go-libp2p/core/peer"
	maddr "github.com/multiformats/go-multiaddr"
	bkeygen "github.com/ordinox/thorchain-tss-lib/ecdsa/keygen"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	tcrypto "github.com/tendermint/tendermint/crypto"

	"github.com/ordinox/thorchain-tss/common"
	"github.com/ordinox/thorchain-tss/conversion"
	"github.com/ordinox/thorchain-tss/keygen"
	"github.com/ordinox/thorchain-tss/keysign"
	"github.com/ordinox/thorchain-tss/messages"
	"github.com/ordinox/thorchain-tss/monitor"
	"github.com/ordinox/thorchain-tss/p2p"
	"github.com/ordinox/thorchain-tss/storage"
)

// TssServer is the structure that can provide all keysign and key gen features
type TssServer struct {
	conf              common.TssConfig
	logger            zerolog.Logger
	p2pCommunication  *p2p.Communication
	localNodePubKey   string
	preParams         *bkeygen.LocalPreParams
	tssKeyGenLocker   *sync.Mutex
	stopChan          chan struct{}
	joinPartyChan     chan struct{}
	partyCoordinator  *p2p.PartyCoordinator
	stateManager      storage.LocalStateManager
	signatureNotifier *keysign.SignatureNotifier
	privateKey        tcrypto.PrivKey
	tssMetrics        *monitor.Metric
}

type PeerInfo struct {
	ID      string
	Address string
}

// NewTss create a new instance of Tss
func NewTss(
	cmdBootstrapPeers []maddr.Multiaddr,
	p2pPort int,
	priKey tcrypto.PrivKey,
	rendezvous,
	baseFolder string,
	conf common.TssConfig,
	preParams *bkeygen.LocalPreParams,
	externalIP string,
) (*TssServer, error) {
	pk := coskey.PubKey{
		Key: priKey.PubKey().Bytes()[:],
	}

	pubKey, err := sdk.MarshalPubKey(sdk.AccPK, &pk)
	if err != nil {
		return nil, fmt.Errorf("fail to genearte the key: %w", err)
	}

	logger := log.With().Str("module", "tss").Logger()
	logger.Info().Msgf("tss bech32 pubkey created, we are: %s", pubKey)

	stateManager, err := storage.NewFileStateMgr(baseFolder)
	if err != nil {
		return nil, fmt.Errorf("fail to create file state manager")
	}

	var bootstrapPeers []maddr.Multiaddr
	savedPeers, err := stateManager.RetrieveP2PAddresses()
	if err != nil {
		bootstrapPeers = cmdBootstrapPeers
	} else {
		bootstrapPeers = savedPeers
		bootstrapPeers = append(bootstrapPeers, cmdBootstrapPeers...)
	}
	comm, err := p2p.NewCommunication(rendezvous, bootstrapPeers, p2pPort, externalIP)
	if err != nil {
		return nil, fmt.Errorf("fail to create communication layer: %w", err)
	}
	// When using the keygen party it is recommended that you pre-compute the
	// "safe primes" and Paillier secret beforehand because this can take some
	// time.
	// This code will generate those parameters using a concurrency limit equal
	// to the number of available CPU cores.
	if preParams == nil || !preParams.Validate() {
		preParams, err = bkeygen.GeneratePreParams(conf.PreParamTimeout)
		if err != nil {
			return nil, fmt.Errorf("fail to generate pre parameters: %w", err)
		}
	}
	if !preParams.Validate() {
		return nil, errors.New("invalid preparams")
	}

	priKeyRawBytes, err := conversion.GetPriKeyRawBytes(priKey)
	if err != nil {
		return nil, fmt.Errorf("fail to get private key")
	}
	if err := comm.Start(priKeyRawBytes); nil != err {
		return nil, fmt.Errorf("fail to start p2p network: %w", err)
	}
	pc := p2p.NewPartyCoordinator(comm.GetHost(), conf.PartyTimeout)
	sn := keysign.NewSignatureNotifier(comm.GetHost())
	metrics := monitor.NewMetric()
	if conf.EnableMonitor {
		metrics.Enable()
	}
	tssServer := TssServer{
		conf:              conf,
		logger:            logger,
		p2pCommunication:  comm,
		localNodePubKey:   pubKey,
		preParams:         preParams,
		tssKeyGenLocker:   &sync.Mutex{},
		stopChan:          make(chan struct{}),
		partyCoordinator:  pc,
		stateManager:      stateManager,
		signatureNotifier: sn,
		privateKey:        priKey,
		tssMetrics:        metrics,
	}

	return &tssServer, nil
}

// Start Tss server
func (t *TssServer) Start() error {
	t.logger.Info().Msg("starting the tss servers")
	return nil
}

// Stop Tss server
func (t *TssServer) Stop() {
	close(t.stopChan)
	// stop the p2p and finish the p2p wait group
	err := t.p2pCommunication.Stop()
	if err != nil {
		t.logger.Error().Msgf("error in shutdown the p2p server")
	}
	t.partyCoordinator.Stop()
	t.logger.Info().Msg("The tss and p2p server has been stopped successfully")
}

func (t *TssServer) setJoinPartyChan(jpc chan struct{}) {
	t.joinPartyChan = jpc
}
func (t *TssServer) unsetJoinPartyChan() {
	t.joinPartyChan = nil
}

func (t *TssServer) notifyJoinPartyChan() {
	if t.joinPartyChan != nil {
		t.joinPartyChan <- struct{}{}
	}
}

func (t *TssServer) requestToMsgId(request interface{}) (string, error) {
	var dat []byte
	var keys []string
	switch value := request.(type) {
	case keygen.Request:
		keys = value.Keys
	case keysign.Request:
		sort.Strings(value.Messages)
		dat = []byte(strings.Join(value.Messages, ","))
		keys = value.SignerPubKeys
	default:
		t.logger.Error().Msg("unknown request type")
		return "", errors.New("unknown request type")
	}
	keyAccumulation := ""
	sort.Strings(keys)
	for _, el := range keys {
		keyAccumulation += el
	}
	dat = append(dat, []byte(keyAccumulation)...)
	return common.MsgToHashString(dat)
}

func (t *TssServer) joinParty(msgID, version string, blockHeight int64, participants []string, threshold int, sigChan chan string) ([]peer.ID, string, error) {
	oldJoinParty, err := conversion.VersionLTCheck(version, messages.NEWJOINPARTYVERSION)
	if err != nil {
		return nil, "", fmt.Errorf("fail to parse the version with error:%w", err)
	}
	if oldJoinParty {
		t.logger.Info().Msg("we apply the leadless join party")
		peerIDs, err := conversion.GetPeerIDsFromPubKeys(participants)
		if err != nil {
			return nil, "NONE", fmt.Errorf("fail to convert pub key to peer id: %w", err)
		}
		var peersIDStr []string
		for _, el := range peerIDs {
			peersIDStr = append(peersIDStr, el.String())
		}
		onlines, err := t.partyCoordinator.JoinPartyWithRetry(msgID, peersIDStr)
		return onlines, "NONE", err
	} else {
		t.logger.Info().Msg("we apply the join party with a leader")

		if len(participants) == 0 {
			t.logger.Error().Msg("we fail to have any participants or passed by request")
			return nil, "", errors.New("no participants can be found")
		}
		peersID, err := conversion.GetPeerIDsFromPubKeys(participants)
		if err != nil {
			return nil, "", errors.New("fail to convert the public key to peer ID")
		}
		var peersIDStr []string
		for _, el := range peersID {
			peersIDStr = append(peersIDStr, el.String())
		}

		return t.partyCoordinator.JoinPartyWithLeader(msgID, blockHeight, peersIDStr, threshold, sigChan)
	}
}

// GetLocalPeerID return the local peer
func (t *TssServer) GetLocalPeerID() string {
	return t.p2pCommunication.GetLocalPeerID()
}

// GetLocalPeerID return the local peer
func (t *TssServer) GetLocalPubKey() string {
	return t.localNodePubKey
}

// GetKnownPeers return the the ID and IP address of all peers.
func (t *TssServer) GetKnownPeers() []PeerInfo {
	infos := []PeerInfo{}
	host := t.p2pCommunication.GetHost()

	for _, conn := range host.Network().Conns() {
		peer := conn.RemotePeer()
		addrs := conn.RemoteMultiaddr()
		ip, _ := addrs.ValueForProtocol(maddr.P_IP4)
		pi := PeerInfo{
			ID:      peer.String(),
			Address: ip,
		}
		infos = append(infos, pi)
	}
	return infos
}
