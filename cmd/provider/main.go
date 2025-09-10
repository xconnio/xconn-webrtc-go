package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/wamp-webrtc-go"
	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/wampproto-go/serializers"
	"github.com/xconnio/xconn-go"
)

const (
	procedureWebRTCOffer     = "io.xconn.webrtc.offer"
	topicOffererOnCandidate  = "io.xconn.webrtc.offerer.on_candidate"
	topicAnswererOnCandidate = "io.xconn.webrtc.answerer.on_candidate"

	testRealm     = "realm1"
	testSecret    = "hello"
	testTicket    = "hello"
	testPublicKey = "f0e3cff77bd851015a99d873e302803d83e693cde41ffe545b26124713bdb08b"
)

type Authenticator struct{}

func NewAuthenticator() *Authenticator {
	return &Authenticator{}
}

func (a *Authenticator) Methods() []auth.Method {
	return []auth.Method{auth.MethodAnonymous, auth.MethodTicket, auth.MethodCRA, auth.MethodCryptoSign}
}

func (a *Authenticator) Authenticate(request auth.Request) (auth.Response, error) {
	switch request.AuthMethod() {
	case auth.MethodAnonymous:
		if request.Realm() == testRealm {
			return auth.NewResponse(request.AuthID(), "anonymous", 0)
		}

		return nil, fmt.Errorf("invalid realm")

	case auth.MethodTicket:
		ticketRequest, ok := request.(*auth.TicketRequest)
		if !ok {
			return nil, fmt.Errorf("invalid request")
		}

		if ticketRequest.Realm() == testRealm && ticketRequest.Ticket() == testTicket {
			return auth.NewResponse(ticketRequest.AuthID(), "anonymous", 0)
		}

		return nil, fmt.Errorf("invalid ticket")

	case auth.MethodCRA:
		if request.Realm() == testRealm {
			return auth.NewCRAResponse(request.AuthID(), "anonymous", testSecret, 0), nil
		}

		return nil, fmt.Errorf("invalid realm")

	case auth.MethodCryptoSign:
		cryptosignRequest, ok := request.(*auth.RequestCryptoSign)
		if !ok {
			return nil, fmt.Errorf("invalid request")
		}

		if cryptosignRequest.Realm() == testRealm && cryptosignRequest.PublicKey() == testPublicKey {
			return auth.NewResponse(cryptosignRequest.AuthID(), "anonymous", 0)
		}

		return nil, fmt.Errorf("unknown publickey")

	default:
		return nil, fmt.Errorf("unknown authentication method: %v", request.AuthMethod())
	}
}

func main() {
	session, err := xconn.ConnectAnonymous(context.Background(), "ws://localhost:8080/ws", "realm1")
	if err != nil {
		log.Fatal("Failed to connect to server:", err)
	}

	webRtcManager := wamp_webrtc_go.NewWebRTCHandler()
	cfg := &wamp_webrtc_go.ProviderConfig{
		Session:                     session,
		ProcedureHandleOffer:        procedureWebRTCOffer,
		TopicHandleRemoteCandidates: topicAnswererOnCandidate,
		TopicPublishLocalCandidate:  topicOffererOnCandidate,
		Serializer:                  &serializers.CBORSerializer{},
		Authenticator:               NewAuthenticator(),
	}
	if err := webRtcManager.Setup(cfg); err != nil {
		log.Fatal("Failed to setup webRtc provider:", err)
	}

	// Close server if SIGINT (CTRL-c) received.
	closeChan := make(chan os.Signal, 1)
	signal.Notify(closeChan, os.Interrupt)

	select {
	case <-closeChan:
	case <-session.Done():
	}
}
