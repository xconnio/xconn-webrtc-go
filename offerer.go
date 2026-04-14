package xconnwebrtc

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/xconn-go"
)

type Offerer struct {
	connection *webrtc.PeerConnection
	channel    chan *webrtc.DataChannel

	publishICECandidate func(topic string, requestID string, candidate string) error
	trickleTopic        string
	trickleRequestID    string
	pendingCandidates   []webrtc.ICECandidateInit

	sync.Mutex
}

func NewOfferer() *Offerer {
	return &Offerer{
		channel: make(chan *webrtc.DataChannel, 1),
	}
}

func (o *Offerer) Offer(offerConfig *OfferConfig) (*Offer, error) {
	const trickleAfter = 100 * time.Millisecond
	end := time.Now().Add(trickleAfter)

	config := webrtc.Configuration{
		ICEServers:           offerConfig.ICEServers,
		ICECandidatePoolSize: 10,
	}

	// Create a new RTCPeerConnection
	peerConnection, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return nil, err
	}

	o.connection = peerConnection

	options := &webrtc.DataChannelInit{
		Ordered:  &offerConfig.Ordered,
		Protocol: &offerConfig.Protocol,
		ID:       &offerConfig.ID,
	}
	dc, err := peerConnection.CreateDataChannel("data", options)
	if err != nil {
		return nil, err
	}

	dc.OnOpen(func() {
		o.channel <- dc
	})

	// Set the handler for Peer connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Debugf("Peer Connection State has changed: %s\n", s.String())
	})

	done := make(chan struct{}, 1)
	var trickle bool
	var initialCandidates []webrtc.ICECandidateInit

	peerConnection.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}

		if !trickle && !time.Now().After(end) {
			initialCandidates = append(initialCandidates, c.ToJSON())
			// First non-host candidate signals end of the fast host phase;
			// everything after goes through the trickle path.
			if c.Typ != webrtc.ICECandidateTypeHost {
				trickle = true
				select {
				case done <- struct{}{}:
				default:
				}
			}
		} else {
			o.handleICECandidate(c.ToJSON())
		}
	})

	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		return nil, err
	}

	// Set the offer as the local description
	err = peerConnection.SetLocalDescription(offer)
	if err != nil {
		return nil, err
	}

	select {
	case <-done:
	case <-time.After(time.Until(end)):
	}

	return &Offer{
		Description: offer,
		Candidates:  initialCandidates,
	}, nil
}

func (o *Offerer) StartICETrickle(session *xconn.Session, topic string, requestID string) {
	o.Lock()
	o.publishICECandidate = func(topic string, requestID string, candidate string) error {
		return session.Publish(topic).Args(requestID, candidate).Do().Err
	}
	o.trickleTopic = topic
	o.trickleRequestID = requestID
	pendingCandidates := append([]webrtc.ICECandidateInit(nil), o.pendingCandidates...)
	o.pendingCandidates = nil
	o.Unlock()

	for _, candidate := range pendingCandidates {
		o.publishCandidate(topic, requestID, candidate)
	}
}

func (o *Offerer) HandleAnswer(answer Answer) error {
	if err := o.connection.SetRemoteDescription(answer.Description); err != nil {
		return err
	}

	for _, candidate := range answer.Candidates {
		if err := o.connection.AddICECandidate(candidate); err != nil {
			return err
		}
	}

	return nil
}

func (o *Offerer) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	return o.connection.AddICECandidate(candidate)
}

func (o *Offerer) WaitReady() chan *webrtc.DataChannel {
	return o.channel
}

func (o *Offerer) handleICECandidate(candidate webrtc.ICECandidateInit) {
	o.Lock()
	canPublish := o.publishICECandidate != nil && o.trickleTopic != "" && o.trickleRequestID != ""
	if !canPublish {
		o.pendingCandidates = append(o.pendingCandidates, candidate)
		o.Unlock()
		return
	}

	topic := o.trickleTopic
	requestID := o.trickleRequestID
	o.Unlock()

	o.publishCandidate(topic, requestID, candidate)
}

func (o *Offerer) publishCandidate(topic string, requestID string, candidate webrtc.ICECandidateInit) {
	candidateData, err := json.Marshal(candidate)
	if err != nil {
		log.Errorf("failed to marshal candidate: %v", err)
		return
	}

	o.Lock()
	publish := o.publishICECandidate
	o.Unlock()
	if publish == nil {
		return
	}

	if err := publish(topic, requestID, string(candidateData)); err != nil {
		log.Errorf("failed to publish ice candidate: %v", err)
	}
}
