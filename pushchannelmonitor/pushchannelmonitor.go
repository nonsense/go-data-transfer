package pushchannelmonitor

import (
	"context"
	"fmt"
	"sync"
	"time"

	logging "github.com/ipfs/go-log/v2"
	"golang.org/x/xerrors"

	datatransfer "github.com/filecoin-project/go-data-transfer"
	"github.com/filecoin-project/go-data-transfer/channels"
)

var log = logging.Logger("dt-pushchanmon")

type monitorAPI interface {
	SubscribeToEvents(subscriber datatransfer.Subscriber) datatransfer.Unsubscribe
	RestartDataTransferChannel(ctx context.Context, chid datatransfer.ChannelID) error
	CloseDataTransferChannelWithError(ctx context.Context, chid datatransfer.ChannelID, cherr error) error
}

// Monitor watches the data-rate for push channels, and restarts
// a channel if the data-rate falls too low
type Monitor struct {
	ctx  context.Context
	stop context.CancelFunc
	mgr  monitorAPI
	cfg  *Config

	lk       sync.RWMutex
	channels map[*monitoredChannel]struct{}
}

type Config struct {
	// Max time to wait for other side to accept push before attempting restart
	AcceptTimeout time.Duration
	// Interval between checks of transfer rate
	Interval time.Duration
	// Min bytes that must be sent in interval
	MinBytesSent uint64
	// Number of times to check transfer rate per interval
	ChecksPerInterval uint32
	// Backoff after restarting
	RestartBackoff time.Duration
	// Number of times to try to restart before failing
	MaxConsecutiveRestarts uint32
	// Max time to wait for the responder to send a Complete message once all
	// data has been sent
	CompleteTimeout time.Duration
}

func NewMonitor(mgr monitorAPI, cfg *Config) *Monitor {
	checkConfig(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	return &Monitor{
		ctx:      ctx,
		stop:     cancel,
		mgr:      mgr,
		cfg:      cfg,
		channels: make(map[*monitoredChannel]struct{}),
	}
}

func checkConfig(cfg *Config) {
	if cfg == nil {
		return
	}

	prefix := "data-transfer channel push monitor config "
	if cfg.AcceptTimeout <= 0 {
		panic(fmt.Sprintf(prefix+"AcceptTimeout is %s but must be > 0", cfg.AcceptTimeout))
	}
	if cfg.Interval <= 0 {
		panic(fmt.Sprintf(prefix+"Interval is %s but must be > 0", cfg.Interval))
	}
	if cfg.ChecksPerInterval == 0 {
		panic(fmt.Sprintf(prefix+"ChecksPerInterval is %d but must be > 0", cfg.ChecksPerInterval))
	}
	if cfg.MinBytesSent == 0 {
		panic(fmt.Sprintf(prefix+"MinBytesSent is %d but must be > 0", cfg.MinBytesSent))
	}
	if cfg.MaxConsecutiveRestarts == 0 {
		panic(fmt.Sprintf(prefix+"MaxConsecutiveRestarts is %d but must be > 0", cfg.MaxConsecutiveRestarts))
	}
	if cfg.CompleteTimeout <= 0 {
		panic(fmt.Sprintf(prefix+"CompleteTimeout is %s but must be > 0", cfg.CompleteTimeout))
	}
}

// AddChannel adds a channel to the push channel monitor
func (m *Monitor) AddChannel(chid datatransfer.ChannelID) *monitoredChannel {
	if !m.enabled() {
		return nil
	}

	m.lk.Lock()
	defer m.lk.Unlock()

	mpc := newMonitoredChannel(m.mgr, chid, m.cfg, m.onMonitoredChannelShutdown)
	m.channels[mpc] = struct{}{}
	return mpc
}

func (m *Monitor) Shutdown() {
	// Causes the run loop to exit
	m.stop()
}

// onShutdown shuts down all monitored channels. It is called when the run
// loop exits.
func (m *Monitor) onShutdown() {
	m.lk.RLock()
	defer m.lk.RUnlock()

	for ch := range m.channels {
		ch.Shutdown()
	}
}

// onMonitoredChannelShutdown is called when a monitored channel shuts down
func (m *Monitor) onMonitoredChannelShutdown(mpc *monitoredChannel) {
	m.lk.Lock()
	defer m.lk.Unlock()

	delete(m.channels, mpc)
}

// enabled indicates whether the push channel monitor is running
func (m *Monitor) enabled() bool {
	return m.cfg != nil
}

func (m *Monitor) Start() {
	if !m.enabled() {
		return
	}

	go m.run()
}

func (m *Monitor) run() {
	defer m.onShutdown()

	// Check data-rate ChecksPerInterval times per interval
	tickInterval := m.cfg.Interval / time.Duration(m.cfg.ChecksPerInterval)
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	log.Infof("Starting push channel monitor with "+
		"%d checks per %s interval (check interval %s); min bytes per interval: %d, restart backoff: %s; max consecutive restarts: %d",
		m.cfg.ChecksPerInterval, m.cfg.Interval, tickInterval, m.cfg.MinBytesSent, m.cfg.RestartBackoff, m.cfg.MaxConsecutiveRestarts)

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.checkDataRate()
		}
	}
}

// check data rate for all monitored channels
func (m *Monitor) checkDataRate() {
	m.lk.RLock()
	defer m.lk.RUnlock()

	for ch := range m.channels {
		ch.checkDataRate()
	}
}

// monitoredChannel keeps track of the data-rate for a push channel, and
// restarts the channel if the rate falls below the minimum allowed
type monitoredChannel struct {
	ctx        context.Context
	cancel     context.CancelFunc
	mgr        monitorAPI
	chid       datatransfer.ChannelID
	cfg        *Config
	unsub      datatransfer.Unsubscribe
	onShutdown func(*monitoredChannel)
	shutdownLk sync.Mutex

	statsLk             sync.RWMutex
	queued              uint64
	sent                uint64
	dataRatePoints      chan *dataRatePoint
	consecutiveRestarts int

	restartLk   sync.RWMutex
	restartedAt time.Time
}

func newMonitoredChannel(
	mgr monitorAPI,
	chid datatransfer.ChannelID,
	cfg *Config,
	onShutdown func(*monitoredChannel),
) *monitoredChannel {
	ctx, cancel := context.WithCancel(context.Background())
	mpc := &monitoredChannel{
		ctx:            ctx,
		cancel:         cancel,
		mgr:            mgr,
		chid:           chid,
		cfg:            cfg,
		onShutdown:     onShutdown,
		dataRatePoints: make(chan *dataRatePoint, cfg.ChecksPerInterval),
	}
	mpc.start()
	return mpc
}

// Cancel the context and unsubscribe from events
func (mc *monitoredChannel) Shutdown() {
	mc.shutdownLk.Lock()
	defer mc.shutdownLk.Unlock()

	// Check if the channel was already shut down
	if mc.cancel == nil {
		return
	}
	mc.cancel() // cancel context so all go-routines exit
	mc.cancel = nil

	// unsubscribe from data transfer events
	mc.unsub()

	// Inform the Manager that this channel has shut down
	go mc.onShutdown(mc)
}

func (mc *monitoredChannel) start() {
	// Prevent shutdown until after startup
	mc.shutdownLk.Lock()
	defer mc.shutdownLk.Unlock()

	log.Debugf("%s: starting push channel data-rate monitoring", mc.chid)

	// Watch to make sure the responder accepts the channel in time
	cancelAcceptTimer := mc.watchForResponderAccept()

	// Watch for data rate events
	mc.unsub = mc.mgr.SubscribeToEvents(func(event datatransfer.Event, channelState datatransfer.ChannelState) {
		if channelState.ChannelID() != mc.chid {
			return
		}

		mc.statsLk.Lock()
		defer mc.statsLk.Unlock()

		// Once the channel completes, shut down the monitor
		state := channelState.Status()
		if channels.IsChannelCleaningUp(state) || channels.IsChannelTerminated(state) {
			log.Debugf("%s: stopping push channel data-rate monitoring", mc.chid)
			go mc.Shutdown()
			return
		}

		switch event.Code {
		case datatransfer.Accept:
			// The Accept event is fired when we receive an Accept message from the responder
			cancelAcceptTimer()
		case datatransfer.Error:
			// If there's an error, attempt to restart the channel
			log.Debugf("%s: data transfer error, restarting", mc.chid)
			go mc.restartChannel()
		case datatransfer.DataQueued:
			// Keep track of the amount of data queued
			mc.queued = channelState.Queued()
		case datatransfer.DataSent:
			// Keep track of the amount of data sent
			mc.sent = channelState.Sent()
			// Some data was sent so reset the consecutive restart counter
			mc.consecutiveRestarts = 0
		case datatransfer.FinishTransfer:
			// The client has finished sending all data. Watch to make sure
			// that the responder sends a message to acknowledge that the
			// transfer is complete
			go mc.watchForResponderComplete()
		}
	})
}

// watchForResponderAccept watches to make sure that the responder sends
// an Accept to our open channel request before the accept timeout.
// Returns a function that can be used to cancel the timer.
func (mc *monitoredChannel) watchForResponderAccept() func() {
	// Start a timer for the accept timeout
	timer := time.NewTimer(mc.cfg.AcceptTimeout)

	go func() {
		defer timer.Stop()

		select {
		case <-mc.ctx.Done():
		case <-timer.C:
			// Timer expired before we received an Accept from the responder,
			// fail the data transfer
			err := xerrors.Errorf("%s: timed out waiting %s for Accept message from remote peer",
				mc.chid, mc.cfg.AcceptTimeout)
			mc.closeChannelAndShutdown(err)
		}
	}()

	return func() { timer.Stop() }
}

// Wait up to the configured timeout for the responder to send a Complete message
func (mc *monitoredChannel) watchForResponderComplete() {
	// Start a timer for the complete timeout
	timer := time.NewTimer(mc.cfg.CompleteTimeout)
	defer timer.Stop()

	select {
	case <-mc.ctx.Done():
		// When the Complete message is received, the channel shuts down
	case <-timer.C:
		// Timer expired before we received a Complete from the responder
		err := xerrors.Errorf("%s: timed out waiting %s for Complete message from remote peer",
			mc.chid, mc.cfg.AcceptTimeout)
		mc.closeChannelAndShutdown(err)
	}
}

type dataRatePoint struct {
	pending uint64
	sent    uint64
}

// check if the amount of data sent in the interval was too low, and if so
// restart the channel
func (mc *monitoredChannel) checkDataRate() {
	mc.statsLk.Lock()
	defer mc.statsLk.Unlock()

	// Before returning, add the current data rate stats to the queue
	defer func() {
		var pending uint64
		if mc.queued > mc.sent { // should always be true but just in case
			pending = mc.queued - mc.sent
		}
		mc.dataRatePoints <- &dataRatePoint{
			pending: pending,
			sent:    mc.sent,
		}
	}()

	// Check that there are enough data points that an interval has elapsed
	if len(mc.dataRatePoints) < int(mc.cfg.ChecksPerInterval) {
		log.Debugf("%s: not enough data points to check data rate yet (%d / %d)",
			mc.chid, len(mc.dataRatePoints), mc.cfg.ChecksPerInterval)

		return
	}

	// Pop the data point from one interval ago
	atIntervalStart := <-mc.dataRatePoints

	// If there was enough pending data to cover the minimum required amount,
	// and the amount sent was lower than the minimum required, restart the
	// channel
	sentInInterval := mc.sent - atIntervalStart.sent
	log.Debugf("%s: since last check: sent: %d - %d = %d, pending: %d, required %d",
		mc.chid, mc.sent, atIntervalStart.sent, sentInInterval, atIntervalStart.pending, mc.cfg.MinBytesSent)
	if atIntervalStart.pending > sentInInterval && sentInInterval < mc.cfg.MinBytesSent {
		go mc.restartChannel()
	}
}

func (mc *monitoredChannel) restartChannel() {
	// Check if the channel is already being restarted
	mc.restartLk.Lock()
	restartedAt := mc.restartedAt
	if restartedAt.IsZero() {
		mc.restartedAt = time.Now()
	}
	mc.restartLk.Unlock()

	if !restartedAt.IsZero() {
		log.Debugf("%s: restart called but already restarting channel (for %s so far; restart backoff is %s)",
			mc.chid, time.Since(mc.restartedAt), mc.cfg.RestartBackoff)
		return
	}

	mc.statsLk.Lock()
	mc.consecutiveRestarts++
	restartCount := mc.consecutiveRestarts
	mc.statsLk.Unlock()

	if uint32(restartCount) > mc.cfg.MaxConsecutiveRestarts {
		// If no data has been transferred since the last transfer, and we've
		// reached the consecutive restart limit, close the channel and
		// shutdown the monitor
		err := xerrors.Errorf("%s: after %d consecutive restarts failed to reach required data transfer rate", mc.chid, restartCount)
		mc.closeChannelAndShutdown(err)
		return
	}

	// Send a restart message for the channel.
	// Note that at the networking layer there is logic to retry if a network
	// connection cannot be established, so this may take some time.
	log.Infof("%s: sending restart message (%d consecutive restarts)", mc.chid, restartCount)
	err := mc.mgr.RestartDataTransferChannel(mc.ctx, mc.chid)
	if err != nil {
		// If it wasn't possible to restart the channel, close the channel
		// and shut down the monitor
		cherr := xerrors.Errorf("%s: failed to send restart message: %s", mc.chid, err)
		mc.closeChannelAndShutdown(cherr)
	} else if mc.cfg.RestartBackoff > 0 {
		log.Infof("%s: restart message sent successfully, backing off %s before allowing any other restarts",
			mc.chid, mc.cfg.RestartBackoff)
		// Backoff a little time after a restart before attempting another
		select {
		case <-time.After(mc.cfg.RestartBackoff):
		case <-mc.ctx.Done():
		}

		log.Debugf("%s: restart back-off %s complete",
			mc.chid, mc.cfg.RestartBackoff)
	}

	mc.restartLk.Lock()
	mc.restartedAt = time.Time{}
	mc.restartLk.Unlock()
}

func (mc *monitoredChannel) closeChannelAndShutdown(cherr error) {
	log.Errorf("closing data-transfer channel: %s", cherr)
	err := mc.mgr.CloseDataTransferChannelWithError(mc.ctx, mc.chid, cherr)
	if err != nil {
		log.Errorf("error closing data-transfer channel %s: %w", mc.chid, err)
	}

	mc.Shutdown()
}
