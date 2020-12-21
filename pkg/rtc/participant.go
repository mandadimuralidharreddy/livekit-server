package rtc

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/ion-sfu/pkg/buffer"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

const (
	placeholderDataChannel = "_private"
	sdBatchSize            = 20
)

type Participant struct {
	id          string
	peerConn    PeerConnection
	sigConn     SignalConnection
	ctx         context.Context
	cancel      context.CancelFunc
	mediaEngine *webrtc.MediaEngine
	name        string
	state       livekit.ParticipantInfo_State
	bi          *buffer.Interceptor
	rtcpCh      chan []rtcp.Packet
	downTracks  map[string][]*sfu.DownTrack

	lock   sync.RWMutex
	tracks map[string]PublishedTrack // tracks that the peer is publishing
	once   sync.Once

	// callbacks & handlers
	// OnTrackPublished - remote peer added a remoteTrack
	OnTrackPublished func(*Participant, PublishedTrack)
	// OnOffer - offer is ready for remote peer
	OnOffer func(webrtc.SessionDescription)
	// OnIceCandidate - ice candidate discovered for local peer
	OnICECandidate func(c *webrtc.ICECandidateInit)
	OnStateChange  func(p *Participant, oldState livekit.ParticipantInfo_State)
	OnClose        func(*Participant)
}

func NewPeerConnection(conf *WebRTCConfig) (*webrtc.PeerConnection, error) {
	me := &webrtc.MediaEngine{}
	me.RegisterDefaultCodecs()
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(conf.SettingEngine))
	return api.NewPeerConnection(conf.Configuration)
}

func NewParticipant(pc PeerConnection, sc SignalConnection, name string) (*Participant, error) {
	me := &webrtc.MediaEngine{}
	me.RegisterDefaultCodecs()

	bi := buffer.NewBufferInterceptor()
	ir := &interceptor.Registry{}
	ir.Add(bi)

	ctx, cancel := context.WithCancel(context.Background())
	participant := &Participant{
		id:          utils.NewGuid(utils.ParticipantPrefix),
		name:        name,
		peerConn:    pc,
		sigConn:     sc,
		ctx:         ctx,
		cancel:      cancel,
		bi:          bi,
		rtcpCh:      make(chan []rtcp.Packet, 10),
		downTracks:  make(map[string][]*sfu.DownTrack),
		state:       livekit.ParticipantInfo_JOINING,
		lock:        sync.RWMutex{},
		tracks:      make(map[string]PublishedTrack, 0),
		mediaEngine: me,
	}

	log := logger.GetLogger()

	pc.OnTrack(participant.onMediaTrack)

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}

		ci := c.ToJSON()

		// write candidate
		err := sc.WriteResponse(&livekit.SignalResponse{
			Message: &livekit.SignalResponse_Trickle{
				Trickle: ToProtoTrickle(ci),
			},
		})
		if err != nil {
			log.Errorw("could not send trickle", "err", err)
		}

		if participant.OnICECandidate != nil {
			participant.OnICECandidate(&ci)
		}
	})

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		logger.GetLogger().Debugw("ICE connection state changed", "state", state.String())
		if state == webrtc.ICEConnectionStateConnected {
			participant.updateState(livekit.ParticipantInfo_ACTIVE)
		}
	})

	// TODO: handle data channel
	pc.OnDataChannel(participant.onDataChannel)

	return participant, nil
}

func (p *Participant) ID() string {
	return p.id
}

func (p *Participant) Name() string {
	return p.name
}

func (p *Participant) State() livekit.ParticipantInfo_State {
	return p.state
}

func (p *Participant) ToProto() *livekit.ParticipantInfo {
	info := &livekit.ParticipantInfo{
		Sid:   p.id,
		Name:  p.name,
		State: p.state,
	}

	for _, t := range p.tracks {
		info.Tracks = append(info.Tracks, TrackToProto(t))
	}
	return info
}

// Answer an offer from remote participant
func (p *Participant) Answer(sdp webrtc.SessionDescription) (answer webrtc.SessionDescription, err error) {
	if err = p.peerConn.SetRemoteDescription(sdp); err != nil {
		return
	}

	answer, err = p.peerConn.CreateAnswer(nil)
	if err != nil {
		err = errors.Wrap(err, "could not create answer")
		return
	}

	if err = p.peerConn.SetLocalDescription(answer); err != nil {
		err = errors.Wrap(err, "could not set local description")
		return
	}

	// only set after answered
	p.peerConn.OnNegotiationNeeded(func() {
		logger.GetLogger().Debugw("negotiation needed", "participantId", p.ID())
		offer, err := p.peerConn.CreateOffer(nil)
		if err != nil {
			logger.GetLogger().Errorw("could not create offer", "err", err)
			return
		}

		err = p.peerConn.SetLocalDescription(offer)
		if err != nil {
			logger.GetLogger().Errorw("could not set local description", "err", err)
			return
		}

		logger.GetLogger().Debugw("sending available offer to participant")
		err = p.sigConn.WriteResponse(&livekit.SignalResponse{
			Message: &livekit.SignalResponse_Negotiate{
				Negotiate: ToProtoSessionDescription(offer),
			},
		})
		if err != nil {
			logger.GetLogger().Errorw("could not send offer to peer",
				"err", err)
		}

		if p.OnOffer != nil {
			p.OnOffer(offer)
		}
	})

	err = p.sigConn.WriteResponse(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Answer{
			Answer: ToProtoSessionDescription(answer),
		},
	})

	if p.state == livekit.ParticipantInfo_JOINING {
		p.updateState(livekit.ParticipantInfo_JOINED)
	}
	return
}

// HandleNegotiate when receiving session description from client
func (p *Participant) HandleNegotiate(sd webrtc.SessionDescription) error {
	if err := p.peerConn.SetRemoteDescription(sd); err != nil {
		return errors.Wrap(err, "could not set remote description")
	}

	if sd.Type == webrtc.SDPTypeOffer {
		answer, err := p.peerConn.CreateAnswer(nil)
		if err != nil {
			return errors.Wrap(err, "could not create answer")
		}

		if err = p.peerConn.SetLocalDescription(answer); err != nil {
			return errors.Wrap(err, "could not set local description")
		}

		// send a negotiate response back
		return p.sigConn.WriteResponse(&livekit.SignalResponse{
			Message: &livekit.SignalResponse_Negotiate{
				Negotiate: ToProtoSessionDescription(answer),
			},
		})
	}

	return nil
}

func (p *Participant) SetRemoteDescription(sdp webrtc.SessionDescription) error {
	logger.GetLogger().Debugw("setting remote description", "type", sdp.Type)
	if err := p.peerConn.SetRemoteDescription(sdp); err != nil {
		return errors.Wrap(err, "could not set remote description")
	}
	return nil
}

// AddICECandidate adds candidates for remote peer
func (p *Participant) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	if err := p.peerConn.AddICECandidate(candidate); err != nil {
		return err
	}
	return nil
}

func (p *Participant) addDownTrack(streamId string, dt *sfu.DownTrack) {
	p.lock.Lock()
	p.downTracks[streamId] = append(p.downTracks[streamId], dt)
	p.lock.Unlock()
	dt.OnBind(func() {
		go p.scheduleDownTrackBindingReports(streamId)
	})
}

func (p *Participant) removeDownTrack(streamId string, dt *sfu.DownTrack) {
	p.lock.Lock()
	defer p.lock.Unlock()
	tracks := p.downTracks[streamId]
	newTracks := make([]*sfu.DownTrack, 0, len(tracks))
	for _, track := range tracks {
		if track != dt {
			newTracks = append(newTracks, track)
		}
	}
	p.downTracks[streamId] = newTracks
}

func (p *Participant) Start() {
	p.once.Do(func() {
		go p.rtcpSendWorker()
		go p.downTracksRTCPWorker()
	})
}

func (p *Participant) Close() error {
	if p.ctx.Err() != nil {
		return p.ctx.Err()
	}
	close(p.rtcpCh)
	p.updateState(livekit.ParticipantInfo_DISCONNECTED)
	if p.OnClose != nil {
		p.OnClose(p)
	}
	p.cancel()
	return p.peerConn.Close()
}

// Subscribes otherPeer to all of the tracks
func (p *Participant) AddSubscriber(op *Participant) error {
	p.lock.RLock()
	defer p.lock.RUnlock()

	for _, track := range p.tracks {
		logger.GetLogger().Debugw("subscribing to remoteTrack",
			"srcParticipant", p.ID(),
			"dstParticipant", op.ID(),
			"remoteTrack", track.ID())
		if err := track.AddSubscriber(op); err != nil {
			return err
		}
	}
	return nil
}

func (p *Participant) RemoveSubscriber(peerId string) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	for _, track := range p.tracks {
		track.RemoveSubscriber(peerId)
	}
}

// signal connection methods
func (p *Participant) SendJoinResponse(otherParticipants []*Participant) error {
	// send Join response
	return p.sigConn.WriteResponse(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Join{
			Join: &livekit.JoinResponse{
				Participant:       p.ToProto(),
				OtherParticipants: ToProtoParticipants(otherParticipants),
			},
		},
	})
}

func (p *Participant) SendParticipantUpdate(participants []*livekit.ParticipantInfo) error {
	return p.sigConn.WriteResponse(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Update{
			Update: &livekit.ParticipantUpdate{
				Participants: participants,
			},
		},
	})
}

func (p *Participant) updateState(state livekit.ParticipantInfo_State) {
	if state == p.state {
		return
	}
	oldState := p.state
	p.state = state

	if p.OnStateChange != nil {
		go func() {
			p.OnStateChange(p, oldState)
		}()
	}
}

// when a new remoteTrack is created, creates a Track and adds it to room
func (p *Participant) onMediaTrack(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
	logger.GetLogger().Debugw("remoteTrack added", "participantId", p.ID(), "remoteTrack", track.ID())

	// create Receiver
	receiver := NewReceiver(p.id, rtpReceiver, p.bi)
	mt := NewMediaTrack(p.id, p.rtcpCh, track, receiver)

	p.handleTrackPublished(mt)
}

func (p *Participant) onDataChannel(dc *webrtc.DataChannel) {
	if dc.Label() == placeholderDataChannel {
		return
	}
	logger.GetLogger().Debugw("dataChannel added", "participantId", p.ID(), "label", dc.Label())

	dt := NewDataTrack(p.id, dc)
	p.lock.Lock()
	p.tracks[dt.id] = dt
	p.lock.Unlock()

	dt.Start()

	p.handleTrackPublished(dt)
}

func (p *Participant) handleTrackPublished(track PublishedTrack) {
	p.lock.Lock()
	p.tracks[track.ID()] = track
	p.lock.Unlock()

	track.Start()

	// confirm publication
	p.sigConn.WriteResponse(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_TrackPublished{
			TrackPublished: TrackToProto(track),
		},
	})
	if p.OnTrackPublished != nil {
		go p.OnTrackPublished(p, track)
	}
}

func (p *Participant) scheduleDownTrackBindingReports(streamId string) {
	var sd []rtcp.SourceDescriptionChunk

	p.lock.RLock()
	dts := p.downTracks[streamId]
	for _, dt := range dts {
		if !dt.IsBound() {
			continue
		}
		chunks := dt.CreateSourceDescriptionChunks()
		if chunks != nil {
			sd = append(sd, chunks...)
		}
	}
	p.lock.RUnlock()

	pkts := []rtcp.Packet{
		&rtcp.SourceDescription{Chunks: sd},
	}

	go func() {
		batch := pkts
		i := 0
		for {
			if err := p.peerConn.WriteRTCP(batch); err != nil {
				logger.GetLogger().Debugw("Sending track binding reports",
					"participant", p.id,
					"err", err)
			}
			if i > 5 {
				return
			}
			i++
			time.Sleep(20 * time.Millisecond)
		}
	}()
}

// downTracksRTCPWorker sends SenderReports periodically when the participant is subscribed to
// other tracks in the room.
func (p *Participant) downTracksRTCPWorker() {
	for {
		time.Sleep(5 * time.Second)

		var pkts []rtcp.Packet
		var sd []rtcp.SourceDescriptionChunk
		p.lock.RLock()
		for _, dts := range p.downTracks {
			for _, dt := range dts {
				if !dt.IsBound() {
					continue
				}
				pkts = append(pkts, dt.CreateSenderReport())
				chunks := dt.CreateSourceDescriptionChunks()
				if chunks != nil {
					sd = append(sd, chunks...)
				}
			}
		}
		p.lock.RUnlock()

		// now send in batches of sdBatchSize
		// first batch will contain the sender reports too
		var batch []rtcp.SourceDescriptionChunk
		for len(sd) > 0 {
			size := len(sd)
			if size > sdBatchSize {
				size = sdBatchSize
			}
			batch = sd[:size]
			sd = sd[size:]
			pkts = append(pkts, &rtcp.SourceDescription{Chunks: batch})
			if err := p.peerConn.WriteRTCP(pkts); err != nil {
				if err == io.EOF || err == io.ErrClosedPipe {
					return
				}
				logger.GetLogger().Errorw("could not send downtrack reports",
					"participant", p.id,
					"err", err)
			}
			pkts = pkts[:0]
		}
	}
}

func (p *Participant) rtcpSendWorker() {
	// read from rtcpChan
	for pkts := range p.rtcpCh {
		for _, pkt := range pkts {
			logger.GetLogger().Debugw("writing RTCP", "packet", pkt)
		}
		if err := p.peerConn.WriteRTCP(pkts); err != nil {
			logger.GetLogger().Errorw("could not write RTCP to participant",
				"participant", p.id,
				"err", err)
		}
	}
}
