package main

import (
	"container/list"
	"fmt"
	"io"
	"launchpad.net/wlmetaserver/wlms/packet"
	"log"
	"net"
	"reflect"
	"strings"
	"time"
)

type Server struct {
	acceptedConnections chan io.ReadWriteCloser
	shutdownServer      chan bool
	serverHasShutdown   chan bool
	clients             *list.List
	user_db             UserDb
	motd                string

	clientSendingTimeout time.Duration
	pingCycleTime        time.Duration
}

func (s *Server) Shutdown() error {
	s.shutdownServer <- true
	return nil
}

func (s *Server) WaitTillShutdown() {
	<-s.serverHasShutdown
}

func (s *Server) NrClients() int {
	return s.clients.Len()
}

func (s *Server) SetClientSendingTimeout(d time.Duration) {
	s.clientSendingTimeout = d
}

func (s *Server) SetPingCycleTime(d time.Duration) {
	s.pingCycleTime = d
}

func (s *Server) isLoggedIn(name string) *Client {
	for e := s.clients.Front(); e != nil; e = e.Next() {
		client := e.Value.(*Client)
		if client.Name() == name {
			return client
		}
	}
	return nil
}

func (s *Server) mainLoop() error {
	log.Print("Starting Goroutine: mainLoop")
	for done := false; !done; {
		select {
		case conn, ok := <-s.acceptedConnections:
			if !ok {
				done = true
			} else {
				go s.dealWithClient(NewClient(conn))
			}
		case <-s.shutdownServer:
			done = true
		}
	}
	log.Print("Ending Goroutine: mainLoop")
	s.shutdown()
	return nil
}

func (s *Server) shutdown() error {
	for s.clients.Len() > 0 {
		e := s.clients.Front()
		e.Value.(*Client).Disconnect()
		s.clients.Remove(e)
	}
	close(s.acceptedConnections)
	s.serverHasShutdown <- true
	return nil
}

func (s *Server) dealWithClient(client *Client) {
	log.Print("Starting Goroutine: dealWithClient")
	timeout_channel := make(chan bool)
	startToPingTimer := time.NewTimer(s.pingCycleTime)
	waitingForPong := false

	for done := false; !done; {
		time.AfterFunc(s.clientSendingTimeout, func() {
			timeout_channel <- true
		})
		select {
		case pkg, ok := <-client.DataStream:
			if !ok {
				done = true
				break
			}
			waitingForPong = false
			startToPingTimer.Reset(s.pingCycleTime)

			cmdName, err := pkg.ReadString()
			if err != nil {
				done = true
				break
			}

			handlerFunc := reflect.ValueOf(s).MethodByName(strings.Join([]string{"Handle", cmdName}, ""))
			if handlerFunc.IsValid() {
				handlerFunc := handlerFunc.Interface().(func(*Client, *packet.Packet) (string, bool))
				errString := ""
				errString, done = handlerFunc(client, pkg)
				if errString != "" {
					client.SendPacket("ERROR", cmdName, errString)
				}
			} else {
				log.Printf("%s: Garbage packet %s", client.Name(), cmdName)
				client.SendPacket("ERROR", "GARBAGE_RECEIVED", "INVALID_CMD")
				client.Disconnect()
				done = true
			}
		case <-timeout_channel:
			client.SendPacket("DISCONNECT", "CLIENT_TIMEOUT")
			done = true
		case <-startToPingTimer.C:
			if waitingForPong {
				client.SendPacket("DISCONNECT", "CLIENT_TIMEOUT")
				done = true
				break
			}
			client.SendPacket("PING")
			waitingForPong = true
			startToPingTimer.Reset(s.pingCycleTime)
		}
	}
	client.Disconnect()
	log.Print("Ending Goroutine: dealWithClient")

	for e := s.clients.Front(); e != nil; e = e.Next() {
		if e.Value.(*Client) == client {
			s.clients.Remove(e)
		}
	}
	s.broadcastToConnectedClients("CLIENTS_UPDATE")
}

func (s *Server) HandleCHAT(client *Client, pkg *packet.Packet) (string, bool) {
	message, err := pkg.ReadString()
	if err != nil {
		return err.Error(), false
	}

	// Sanitize message.
	message = strings.Replace(message, "<", "&lt;", -1)
	receiver, err := pkg.ReadString()
	if err != nil {
		return err.Error(), false
	}

	if len(receiver) == 0 {
		s.broadcastToConnectedClients("CHAT", client.Name(), message, "public")
	} else {
		recv_client := s.isLoggedIn(receiver)
		if recv_client != nil {
			recv_client.SendPacket("CHAT", client.Name(), message, "private")
		}
	}
	return "", false
}

func (s *Server) HandleMOTD(client *Client, pkg *packet.Packet) (string, bool) {
	message, err := pkg.ReadString()
	if err != nil {
		return err.Error(), false
	}

	if client.Permissions() != SUPERUSER {
		return "DEFICIENT_PERMISSION", false
	}
	s.motd = message
	s.broadcastToConnectedClients("CHAT", "", s.motd, "system")

	return "", false
}

func (s *Server) HandleDISCONNECT(client *Client, pkg *packet.Packet) (string, bool) {
	reason, err := pkg.ReadString()
	if err != nil {
		return err.Error(), true
	}
	log.Printf("%s: leaving. Reason: '%s'", client.Name(), reason)
	return "", true
}

func (s *Server) HandlePONG(client *Client, pkg *packet.Packet) (string, bool) {
	return "", false
}

func (s *Server) HandleLOGIN(client *Client, pkg *packet.Packet) (string, bool) {
	protocolVersion, err := pkg.ReadInt()
	if err != nil {
		return err.Error(), true
	}
	if protocolVersion != 0 {
		return "UNSUPPORTED_PROTOCOL", true
	}

	userName, err := pkg.ReadString()
	if err != nil {
		return err.Error(), true
	}

	buildId, err := pkg.ReadString()
	if err != nil {
		return err.Error(), true
	}

	isRegisteredOnServer, err := pkg.ReadBool()
	if err != nil {
		return err.Error(), true
	}

	if isRegisteredOnServer {
		if s.isLoggedIn(userName) != nil {
			return "ALREADY_LOGGED_IN", true
		}
		if !s.user_db.ContainsName(userName) {
			return "WRONG_PASSWORD", true
		}
		password, err := pkg.ReadString()
		if err != nil {
			return err.Error(), true
		}
		if !s.user_db.PasswordCorrect(userName, password) {
			return "WRONG_PASSWORD", true
		}
		client.SetPermissions(s.user_db.Permissions(userName))
	} else {
		baseName := userName
		for i := 1; s.user_db.ContainsName(userName) || s.isLoggedIn(userName) != nil; i++ {
			userName = fmt.Sprintf("%s%d", baseName, i)
		}
	}

	client.SetBuildId(buildId)
	client.SetName(userName)
	client.SetLoginTime(time.Now())
	client.SetState(CONNECTED)

	client.SendPacket("LOGIN", userName, client.Permissions().String())
	client.SendPacket("TIME", int(time.Now().Unix()))
	s.clients.PushBack(client)
	s.broadcastToConnectedClients("CLIENTS_UPDATE")

	if len(s.motd) != 0 {
		client.SendPacket("CHAT", "", s.motd, "system")
	}

	return "", false
}

func (s *Server) broadcastToConnectedClients(data ...interface{}) {
	for e := s.clients.Front(); e != nil; e = e.Next() {
		client := e.Value.(*Client)
		if client.State() == CONNECTED {
			client.SendPacket(data...)
		}
	}
}

func listeningLoop(C chan io.ReadWriteCloser) {
	ln, err := net.Listen("tcp", ":7395") // TODO(sirver): softcode this
	if err != nil {
		log.Fatal(err)
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			break
		}
		C <- conn
	}
}
func CreateServer() *Server {
	// NOCOM(sirver): should use a proper database connection or flat file
	C := make(chan io.ReadWriteCloser)
	// NOCOM(sirver): no way to stop the listening loop right now
	go listeningLoop(C)
	return CreateServerUsing(C, NewInMemoryDb())
}

func CreateServerUsing(acceptedConnections chan io.ReadWriteCloser, db UserDb) *Server {
	server := &Server{
		acceptedConnections:  acceptedConnections,
		shutdownServer:       make(chan bool),
		serverHasShutdown:    make(chan bool),
		clients:              list.New(),
		user_db:              db,
		clientSendingTimeout: time.Second * 30,
		pingCycleTime:        time.Second * 15,
	}

	go server.mainLoop()
	return server
}