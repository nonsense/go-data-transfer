package impl

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hannahhoward/go-pubsub"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	logging "github.com/ipfs/go-log/v2"
	"github.com/ipld/go-ipld-prime"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	"github.com/libp2p/go-libp2p-core/peer"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-storedcounter"

	datatransfer "github.com/filecoin-project/go-data-transfer"
	"github.com/filecoin-project/go-data-transfer/channels"
	"github.com/filecoin-project/go-data-transfer/cidlists"
	"github.com/filecoin-project/go-data-transfer/encoding"
	"github.com/filecoin-project/go-data-transfer/message"
	"github.com/filecoin-project/go-data-transfer/network"
	"github.com/filecoin-project/go-data-transfer/pushchannelmonitor"
	"github.com/filecoin-project/go-data-transfer/registry"
)

var log = logging.Logger("dt-impl")

type manager struct {
	dataTransferNetwork   network.DataTransferNetwork
	validatedTypes        *registry.Registry
	resultTypes           *registry.Registry
	revalidators          *registry.Registry
	transportConfigurers  *registry.Registry
	pubSub                *pubsub.PubSub
	readySub              *pubsub.PubSub
	channels              *channels.Channels
	peerID                peer.ID
	transport             datatransfer.Transport
	storedCounter         *storedcounter.StoredCounter
	channelRemoveTimeout  time.Duration
	reconnectsLk          sync.RWMutex
	reconnects            map[datatransfer.ChannelID]chan struct{}
	cidLists              cidlists.CIDLists
	pushChannelMonitor    *pushchannelmonitor.Monitor
	pushChannelMonitorCfg *pushchannelmonitor.Config
}

type internalEvent struct {
	evt   datatransfer.Event
	state datatransfer.ChannelState
}

func dispatcher(evt pubsub.Event, subscriberFn pubsub.SubscriberFn) error {
	ie, ok := evt.(internalEvent)
	if !ok {
		return errors.New("wrong type of event")
	}
	cb, ok := subscriberFn.(datatransfer.Subscriber)
	if !ok {
		return errors.New("wrong type of event")
	}
	cb(ie.evt, ie.state)
	return nil
}

func readyDispatcher(evt pubsub.Event, fn pubsub.SubscriberFn) error {
	migrateErr, ok := evt.(error)
	if !ok && evt != nil {
		return errors.New("wrong type of event")
	}
	cb, ok := fn.(datatransfer.ReadyFunc)
	if !ok {
		return errors.New("wrong type of event")
	}
	cb(migrateErr)
	return nil
}

// DataTransferOption configures the data transfer manager
type DataTransferOption func(*manager)

// ChannelRemoveTimeout sets the timeout after which channels are removed from the manager
func ChannelRemoveTimeout(timeout time.Duration) DataTransferOption {
	return func(m *manager) {
		m.channelRemoveTimeout = timeout
	}
}

// PushChannelRestartConfig sets the configuration options for automatically
// restarting push channels
func PushChannelRestartConfig(cfg pushchannelmonitor.Config) DataTransferOption {
	return func(m *manager) {
		m.pushChannelMonitorCfg = &cfg
	}
}

const defaultChannelRemoveTimeout = 1 * time.Hour

// NewDataTransfer initializes a new instance of a data transfer manager
func NewDataTransfer(ds datastore.Batching, cidListsDir string, dataTransferNetwork network.DataTransferNetwork, transport datatransfer.Transport, storedCounter *storedcounter.StoredCounter, options ...DataTransferOption) (datatransfer.Manager, error) {
	m := &manager{
		dataTransferNetwork:  dataTransferNetwork,
		validatedTypes:       registry.NewRegistry(),
		resultTypes:          registry.NewRegistry(),
		revalidators:         registry.NewRegistry(),
		transportConfigurers: registry.NewRegistry(),
		pubSub:               pubsub.New(dispatcher),
		readySub:             pubsub.New(readyDispatcher),
		peerID:               dataTransferNetwork.ID(),
		transport:            transport,
		storedCounter:        storedCounter,
		channelRemoveTimeout: defaultChannelRemoveTimeout,
		reconnects:           make(map[datatransfer.ChannelID]chan struct{}),
	}

	cidLists, err := cidlists.NewCIDLists(cidListsDir)
	if err != nil {
		return nil, err
	}
	m.cidLists = cidLists
	channels, err := channels.New(ds, cidLists, m.notifier, m.voucherDecoder, m.resultTypes.Decoder, &channelEnvironment{m}, dataTransferNetwork.ID())
	if err != nil {
		return nil, err
	}
	m.channels = channels

	// Apply config options
	for _, option := range options {
		option(m)
	}

	// Start push channel monitor after applying config options as the config
	// options may apply to the monitor
	m.pushChannelMonitor = pushchannelmonitor.NewMonitor(m, m.pushChannelMonitorCfg)
	m.pushChannelMonitor.Start()

	return m, nil
}

func (m *manager) voucherDecoder(voucherType datatransfer.TypeIdentifier) (encoding.Decoder, bool) {
	decoder, has := m.validatedTypes.Decoder(voucherType)
	if !has {
		return m.revalidators.Decoder(voucherType)
	}
	return decoder, true
}

func (m *manager) notifier(evt datatransfer.Event, chst datatransfer.ChannelState) {
	err := m.pubSub.Publish(internalEvent{evt, chst})
	if err != nil {
		log.Warnf("err publishing DT event: %s", err.Error())
	}
}

// Start initializes data transfer processing
func (m *manager) Start(ctx context.Context) error {
	log.Info("start data-transfer module")

	go func() {
		err := m.channels.Start(ctx)
		if err != nil {
			log.Errorf("Migrating data transfer state machines: %s", err.Error())
		}
		err = m.readySub.Publish(err)
		if err != nil {
			log.Warnf("Publish data transfer ready event: %s", err.Error())
		}
	}()

	dtReceiver := &receiver{m}
	m.dataTransferNetwork.SetDelegate(dtReceiver)
	return m.transport.SetEventHandler(m)
}

// OnReady registers a listener for when the data transfer manager has finished starting up
func (m *manager) OnReady(ready datatransfer.ReadyFunc) {
	m.readySub.Subscribe(ready)
}

// Stop terminates all data transfers and ends processing
func (m *manager) Stop(ctx context.Context) error {
	log.Info("stop data-transfer module")
	m.pushChannelMonitor.Shutdown()
	return m.transport.Shutdown(ctx)
}

// RegisterVoucherType registers a validator for the given voucher type
// returns error if:
// * voucher type does not implement voucher
// * there is a voucher type registered with an identical identifier
// * voucherType's Kind is not reflect.Ptr
func (m *manager) RegisterVoucherType(voucherType datatransfer.Voucher, validator datatransfer.RequestValidator) error {
	err := m.validatedTypes.Register(voucherType, validator)
	if err != nil {
		return xerrors.Errorf("error registering voucher type: %w", err)
	}
	return nil
}

// OpenPushDataChannel opens a data transfer that will send data to the recipient peer and
// transfer parts of the piece that match the selector
func (m *manager) OpenPushDataChannel(ctx context.Context, requestTo peer.ID, voucher datatransfer.Voucher, baseCid cid.Cid, selector ipld.Node) (datatransfer.ChannelID, error) {
	log.Infof("open push channel to %s with base cid %s", requestTo, baseCid)

	req, err := m.newRequest(ctx, selector, false, voucher, baseCid, requestTo)
	if err != nil {
		return datatransfer.ChannelID{}, err
	}

	chid, err := m.channels.CreateNew(m.peerID, req.TransferID(), baseCid, selector, voucher,
		m.peerID, m.peerID, requestTo) // initiator = us, sender = us, receiver = them
	if err != nil {
		return chid, err
	}
	processor, has := m.transportConfigurers.Processor(voucher.Type())
	if has {
		transportConfigurer := processor.(datatransfer.TransportConfigurer)
		transportConfigurer(chid, voucher, m.transport)
	}
	m.dataTransferNetwork.Protect(requestTo, chid.String())
	monitoredChan := m.pushChannelMonitor.AddChannel(chid)
	if err := m.dataTransferNetwork.SendMessage(ctx, requestTo, req); err != nil {
		err = fmt.Errorf("Unable to send request: %w", err)
		_ = m.channels.Error(chid, err)

		// If push channel monitoring is enabled, shutdown the monitor as it
		// wasn't possible to start the data transfer
		if monitoredChan != nil {
			monitoredChan.Shutdown()
		}

		return chid, err
	}

	return chid, nil
}

// OpenPullDataChannel opens a data transfer that will request data from the sending peer and
// transfer parts of the piece that match the selector
func (m *manager) OpenPullDataChannel(ctx context.Context, requestTo peer.ID, voucher datatransfer.Voucher, baseCid cid.Cid, selector ipld.Node) (datatransfer.ChannelID, error) {
	log.Infof("open pull channel to %s with base cid %s", requestTo, baseCid)

	req, err := m.newRequest(ctx, selector, true, voucher, baseCid, requestTo)
	if err != nil {
		return datatransfer.ChannelID{}, err
	}
	// initiator = us, sender = them, receiver = us
	chid, err := m.channels.CreateNew(m.peerID, req.TransferID(), baseCid, selector, voucher,
		m.peerID, requestTo, m.peerID)
	if err != nil {
		return chid, err
	}
	processor, has := m.transportConfigurers.Processor(voucher.Type())
	if has {
		transportConfigurer := processor.(datatransfer.TransportConfigurer)
		transportConfigurer(chid, voucher, m.transport)
	}
	m.dataTransferNetwork.Protect(requestTo, chid.String())
	if err := m.transport.OpenChannel(ctx, requestTo, chid, cidlink.Link{Cid: baseCid}, selector, nil, req); err != nil {
		err = fmt.Errorf("Unable to send request: %w", err)
		_ = m.channels.Error(chid, err)
		return chid, err
	}
	return chid, nil
}

// SendVoucher sends an intermediate voucher as needed when the receiver sends a request for revalidation
func (m *manager) SendVoucher(ctx context.Context, channelID datatransfer.ChannelID, voucher datatransfer.Voucher) error {
	chst, err := m.channels.GetByID(ctx, channelID)
	if err != nil {
		return err
	}
	if channelID.Initiator != m.peerID {
		return errors.New("cannot send voucher for request we did not initiate")
	}
	updateRequest, err := message.VoucherRequest(channelID.ID, voucher.Type(), voucher)
	if err != nil {
		return err
	}
	if err := m.dataTransferNetwork.SendMessage(ctx, chst.OtherPeer(), updateRequest); err != nil {
		err = fmt.Errorf("Unable to send request: %w", err)
		_ = m.OnRequestDisconnected(ctx, channelID)
		return err
	}
	return m.channels.NewVoucher(channelID, voucher)
}

// close an open channel (effectively a cancel)
func (m *manager) CloseDataTransferChannel(ctx context.Context, chid datatransfer.ChannelID) error {
	log.Infof("close channel %s", chid)

	chst, err := m.channels.GetByID(ctx, chid)
	if err != nil {
		return err
	}

	// Close the channel on the local transport
	err = m.transport.CloseChannel(ctx, chid)
	if err != nil {
		log.Warnf("unable to close channel %s: %s", chid, err)
	}

	// Send a cancel message to the remote peer
	log.Infof("%s: sending cancel channel to %s for channel %s", m.peerID, chst.OtherPeer(), chid)
	err = m.dataTransferNetwork.SendMessage(ctx, chst.OtherPeer(), m.cancelMessage(chid))
	if err != nil {
		err = fmt.Errorf("unable to send cancel message for channel %s to peer %s: %w",
			chid, m.peerID, err)
		_ = m.OnRequestDisconnected(ctx, chid)
		log.Warn(err)
	}

	// Fire a cancel event
	fsmerr := m.channels.Cancel(chid)
	// If it wasn't possible to send a cancel message to the peer, return
	// that error
	if err != nil {
		return err
	}
	// If it wasn't possible to fire a cancel event, return that error
	if fsmerr != nil {
		return xerrors.Errorf("unable to send cancel to channel FSM: %w", fsmerr)
	}

	return nil
}

// close an open channel and fire an error event
func (m *manager) CloseDataTransferChannelWithError(ctx context.Context, chid datatransfer.ChannelID, cherr error) error {
	log.Infof("close channel %s with error %s", chid, cherr)

	chst, err := m.channels.GetByID(ctx, chid)
	if err != nil {
		return err
	}

	// Cancel the channel on the local transport
	err = m.transport.CloseChannel(ctx, chid)
	if err != nil {
		log.Warnf("unable to close channel %s: %s", chid, err)
	}

	// Try to send a cancel message to the remote peer. It's quite likely
	// we aren't able to send the message to the peer because the channel
	// is already in an error state, which is probably because of connection
	// issues, so if we cant send the message just log a warning.
	log.Infof("%s: sending cancel channel to %s for channel %s", m.peerID, chst.OtherPeer(), chid)
	err = m.dataTransferNetwork.SendMessage(ctx, chst.OtherPeer(), m.cancelMessage(chid))
	if err != nil {
		// Just log a warning here because it's important that we fire the
		// error event with the original error so that it doesn't get masked
		// by subsequent errors.
		log.Warnf("unable to send cancel message for channel %s to peer %s: %w",
			chid, m.peerID, err)
	}

	// Fire an error event
	err = m.channels.Error(chid, cherr)
	if err != nil {
		return xerrors.Errorf("unable to send error %s to channel FSM: %w", cherr, err)
	}

	return nil
}

// pause a running data transfer channel
func (m *manager) PauseDataTransferChannel(ctx context.Context, chid datatransfer.ChannelID) error {
	log.Infof("pause channel %s", chid)

	pausable, ok := m.transport.(datatransfer.PauseableTransport)
	if !ok {
		return datatransfer.ErrUnsupported
	}

	err := pausable.PauseChannel(ctx, chid)
	if err != nil {
		log.Warnf("Error attempting to pause at transport level: %s", err.Error())
	}

	if err := m.dataTransferNetwork.SendMessage(ctx, chid.OtherParty(m.peerID), m.pauseMessage(chid)); err != nil {
		err = fmt.Errorf("Unable to send pause message: %w", err)
		_ = m.OnRequestDisconnected(ctx, chid)
		return err
	}

	return m.pause(chid)
}

// resume a running data transfer channel
func (m *manager) ResumeDataTransferChannel(ctx context.Context, chid datatransfer.ChannelID) error {
	log.Infof("resume channel %s", chid)

	pausable, ok := m.transport.(datatransfer.PauseableTransport)
	if !ok {
		return datatransfer.ErrUnsupported
	}

	err := pausable.ResumeChannel(ctx, m.resumeMessage(chid), chid)
	if err != nil {
		log.Warnf("Error attempting to pause at transport level: %s", err.Error())
	}

	return m.resume(chid)
}

// get channel state
func (m *manager) ChannelState(ctx context.Context, chid datatransfer.ChannelID) (datatransfer.ChannelState, error) {
	return m.channels.GetByID(ctx, chid)
}

// get status of a transfer
func (m *manager) TransferChannelStatus(ctx context.Context, chid datatransfer.ChannelID) datatransfer.Status {
	chst, err := m.channels.GetByID(ctx, chid)
	if err != nil {
		return datatransfer.ChannelNotFoundError
	}
	return chst.Status()
}

// get notified when certain types of events happen
func (m *manager) SubscribeToEvents(subscriber datatransfer.Subscriber) datatransfer.Unsubscribe {
	return datatransfer.Unsubscribe(m.pubSub.Subscribe(subscriber))
}

// get all in progress transfers
func (m *manager) InProgressChannels(ctx context.Context) (map[datatransfer.ChannelID]datatransfer.ChannelState, error) {
	return m.channels.InProgress()
}

// RegisterRevalidator registers a revalidator for the given voucher type
// Note: this is the voucher type used to revalidate. It can share a name
// with the initial validator type and CAN be the same type, or a different type.
// The revalidator can simply be the sampe as the original request validator,
// or a different validator that satisfies the revalidator interface.
func (m *manager) RegisterRevalidator(voucherType datatransfer.Voucher, revalidator datatransfer.Revalidator) error {
	err := m.revalidators.Register(voucherType, revalidator)
	if err != nil {
		return xerrors.Errorf("error registering revalidator type: %w", err)
	}
	return nil
}

// RegisterVoucherResultType allows deserialization of a voucher result,
// so that a listener can read the metadata
func (m *manager) RegisterVoucherResultType(resultType datatransfer.VoucherResult) error {
	err := m.resultTypes.Register(resultType, nil)
	if err != nil {
		return xerrors.Errorf("error registering voucher type: %w", err)
	}
	return nil
}

// RegisterTransportConfigurer registers the given transport configurer to be run on requests with the given voucher
// type
func (m *manager) RegisterTransportConfigurer(voucherType datatransfer.Voucher, configurer datatransfer.TransportConfigurer) error {
	err := m.transportConfigurers.Register(voucherType, configurer)
	if err != nil {
		return xerrors.Errorf("error registering transport configurer: %w", err)
	}
	return nil
}

// RestartDataTransferChannel restarts data transfer on the channel with the given channelId
func (m *manager) RestartDataTransferChannel(ctx context.Context, chid datatransfer.ChannelID) error {
	log.Infof("restart channel %s", chid)

	channel, err := m.channels.GetByID(ctx, chid)
	if err != nil {
		return xerrors.Errorf("failed to fetch channel: %w", err)
	}

	// if channel has already been completed, there is nothing to do.
	// TODO We could be in a state where the channel has completed but the corresponding event hasnt fired in the client/provider.
	if channels.IsChannelTerminated(channel.Status()) {
		return nil
	}

	// if channel is is cleanup state, finish it
	if channels.IsChannelCleaningUp(channel.Status()) {
		return m.channels.CompleteCleanupOnRestart(channel.ChannelID())
	}

	// initiate restart
	chType := m.channelDataTransferType(channel)
	switch chType {
	case ManagerPeerReceivePush:
		return m.restartManagerPeerReceivePush(ctx, channel)
	case ManagerPeerReceivePull:
		return m.restartManagerPeerReceivePull(ctx, channel)
	case ManagerPeerCreatePull:
		return m.openPullRestartChannel(ctx, channel)
	case ManagerPeerCreatePush:
		return m.openPushRestartChannel(ctx, channel)
	}

	return nil
}

func (m *manager) channelDataTransferType(channel datatransfer.ChannelState) ChannelDataTransferType {
	initiator := channel.ChannelID().Initiator
	if channel.IsPull() {
		// we created a pull channel
		if initiator == m.peerID {
			return ManagerPeerCreatePull
		}

		// we received a pull channel
		return ManagerPeerReceivePull
	}

	// we created a push channel
	if initiator == m.peerID {
		return ManagerPeerCreatePush
	}

	// we received a push channel
	return ManagerPeerReceivePush
}
