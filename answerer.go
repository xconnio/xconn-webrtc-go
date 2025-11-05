package xconnwebrtc

import (
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"
)

type Answerer struct {
	connection *webrtc.PeerConnection
	channel    chan *webrtc.DataChannel

	onIceCandidate   func(candidate *webrtc.ICECandidate)
	cachedCandidates []webrtc.ICECandidateInit

	sync.Mutex
}

func NewAnswerer() *Answerer {
	return &Answerer{
		channel: make(chan *webrtc.DataChannel, 1),
	}
}

func (a *Answerer) Answer(answerConfig *AnswerConfig, offer Offer, trickleAfter time.Duration) (*Answer, error) {
	start := time.Now()
	end := start.Add(trickleAfter)

	config := webrtc.Configuration{
		ICEServers:           answerConfig.ICEServers,
		ICECandidatePoolSize: 10,
	}

	connection, err := webrtc.NewPeerConnection(config)
	if err != nil {
		return nil, err
	}

	a.Lock()
	a.connection = connection
	a.Unlock()

	if err = connection.SetRemoteDescription(offer.Description); err != nil {
		return nil, err
	}

	for _, candidate := range offer.Candidates {
		if err = connection.AddICECandidate(candidate); err != nil {
			return nil, err
		}
	}

	a.Lock()
	for _, candidate := range a.cachedCandidates {
		if err = connection.AddICECandidate(candidate); err != nil {
			log.Errorf("failed to add ice candidate: %v", err)
		}
	}

	a.cachedCandidates = nil
	a.Unlock()

	done := make(chan struct{})
	var trickle = false
	var initialCandidates []webrtc.ICECandidateInit
	connection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			log.Debugf("ice candidates gathering took %s", time.Since(start))
			return
		}

		if trickle || time.Now().After(end) {
			a.onIceCandidate(candidate)
		} else {
			initialCandidates = append(initialCandidates, candidate.ToJSON())
			// host candidate gathering is done, any further candidates should
			// be signaled with Trickle ICE.
			if candidate.Typ != webrtc.ICECandidateTypeHost {
				trickle = true
				done <- struct{}{}
			}
		}
	})

	var once sync.Once
	connection.OnDataChannel(func(d *webrtc.DataChannel) {
		once.Do(func() { a.channel <- d })
	})

	answer, err := connection.CreateAnswer(nil)
	if err != nil {
		return nil, err
	}

	if err = connection.SetLocalDescription(answer); err != nil {
		return nil, err
	}

	select {
	case <-done:
	case <-time.After(time.Until(end)):
	}

	return &Answer{
		Candidates:  initialCandidates,
		Description: answer,
	}, nil
}

func (a *Answerer) OnIceCandidate(callback func(candidate *webrtc.ICECandidate)) {
	a.Lock()
	defer a.Unlock()

	a.onIceCandidate = callback
}

func (a *Answerer) AddICECandidate(candidate webrtc.ICECandidateInit) error {
	a.Lock()
	defer a.Unlock()

	if a.connection == nil {
		a.cachedCandidates = append(a.cachedCandidates, candidate)
		return nil
	} else {
		return a.connection.AddICECandidate(candidate)
	}
}

func (a *Answerer) WaitReady() chan *webrtc.DataChannel {
	return a.channel
}
