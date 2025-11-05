package xconnwebrtc

import (
	"encoding/json"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"

	"github.com/xconnio/xconn-go"
)

type Offerer struct {
	connection *webrtc.PeerConnection
	channel    chan *webrtc.DataChannel
}

func NewOfferer() *Offerer {
	return &Offerer{
		channel: make(chan *webrtc.DataChannel, 1),
	}
}

func (o *Offerer) Offer(offerConfig *OfferConfig, session *xconn.Session, requestID string) (*Offer, error) {
	// Prepare the configuration
	config := webrtc.Configuration{
		ICEServers: offerConfig.ICEServers,
	}

	// Create a new RTCPeerConnection
	peerConnection, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return nil, err
	}

	peerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate != nil {
			answerData, err := json.Marshal(candidate.ToJSON())
			if err != nil {
				log.Errorf("failed to marshal answer: %v", err)
				return
			}

			_ = session.Publish(offerConfig.TopicAnswererOnCandidate).Args(requestID, string(answerData)).Do()
		}
	})

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
