package xconnwebrtc

import (
	"encoding/json"
	"sync"

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
	// Prepare the configuration
	config := webrtc.Configuration{
		ICEServers: offerConfig.ICEServers,
	}

	// Create a new RTCPeerConnection
	peerConnection, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return nil, err
	}

	o.connection = peerConnection
	peerConnection.OnICECandidate(o.onICECandidate)

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

	// Create a new offer
	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		return nil, err
	}

	// Set the offer as the local description
	err = peerConnection.SetLocalDescription(offer)
	if err != nil {
		return nil, err
	}

	return &Offer{
		Description: offer,
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

func (o *Offerer) onICECandidate(candidate *webrtc.ICECandidate) {
	if candidate == nil {
		return
	}

	o.handleICECandidate(candidate.ToJSON())
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
