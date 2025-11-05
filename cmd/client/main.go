package main

import (
	"context"

	log "github.com/sirupsen/logrus"

	"github.com/xconnio/wampproto-go/auth"
	"github.com/xconnio/xconn-go"
	"github.com/xconnio/xconn-webrtc-go"
)

const (
	procedureWebRTCOffer     = "io.xconn.webrtc.offer"
	topicAnswererOnCandidate = "io.xconn.webrtc.answerer.on_candidate"
	topicOffererOnCandidate  = "io.xconn.webrtc.offerer.on_candidate"
)

func main() {
	session, err := xconn.ConnectAnonymous(context.Background(), "ws://localhost:8080/ws", "realm1")
	if err != nil {
		log.Fatal("Failed to connect to server:", err)
	}

	config := &xconnwebrtc.ClientConfig{
		Realm:                    "realm1",
		ProcedureWebRTCOffer:     procedureWebRTCOffer,
		TopicAnswererOnCandidate: topicAnswererOnCandidate,
		TopicOffererOnCandidate:  topicOffererOnCandidate,
		Serializer:               xconn.CBORSerializerSpec,
		Authenticator:            auth.NewWAMPCRAAuthenticator("john", "hello", map[string]any{}),
		Session:                  session,
	}
	webRTCSession, err := xconnwebrtc.ConnectWebRTC(config)
	if err != nil {
		log.Fatal(err)
	}

	log.Println(webRTCSession)
}
