/*
Copyright 2015 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package srv

import (
	"context"
	"fmt"
	"io"
	//"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/pam"
	"github.com/gravitational/teleport/lib/services"
	rsession "github.com/gravitational/teleport/lib/session"
	"github.com/gravitational/teleport/lib/sshutils"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"
)

var ctxID int32

// Server is regular or forwarding SSH server.
type Server interface {
	// ID is the unique ID of the server.
	ID() string

	// GetNamespace returns the namespace the server was created in.
	GetNamespace() string

	// AdvertiseAddr is the publicly addressable address of this server.
	AdvertiseAddr() string

	// Component is the type of server, forwarding or regular.
	Component() string

	// PermitUserEnvironment returns if reading environment variables upon
	// startup is allowed.
	PermitUserEnvironment() bool

	// EmitAuditEvent emits an Audit Event to the Auth Server.
	EmitAuditEvent(string, events.EventFields)

	// GetAuditLog returns the Audit Log for this cluster.
	GetAuditLog() events.IAuditLog

	// GetAccessPoint returns an auth.AccessPoint for this cluster.
	GetAccessPoint() auth.AccessPoint

	// GetSessionServer returns a session server.
	GetSessionServer() rsession.Service

	// GetDataDir returns data directory of the server
	GetDataDir() string

	// GetPAM returns PAM configuration for this server.
	GetPAM() (*pam.Config, error)

	// GetClock returns a clock setup for the server
	GetClock() clockwork.Clock

	// GetInfo returns a services.Server that represents this server.
	GetInfo() services.Server
}

// IdentityContext holds all identity information associated with the user
// logged on the connection.
type IdentityContext struct {
	// TeleportUser is the Teleport user associated with the connection.
	TeleportUser string

	// Login is the operating system user associated with the connection.
	Login string

	// Certificate is the SSH user certificate bytes marshalled in the OpenSSH
	// authorized_keys format.
	Certificate []byte

	// CertAuthority is the Certificate Authority that signed the Certificate.
	CertAuthority services.CertAuthority

	// RoleSet is the roles this Teleport user is associated with. RoleSet is
	// used to check RBAC permissions.
	RoleSet services.RoleSet

	// CertValidBefore is set to the expiry time of a certificate, or
	// empty, if cert does not expire
	CertValidBefore time.Time
}

// GetCertificate parses the SSH certificate bytes and returns a *ssh.Certificate.
func (c IdentityContext) GetCertificate() (*ssh.Certificate, error) {
	k, _, _, _, err := ssh.ParseAuthorizedKey(c.Certificate)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	cert, ok := k.(*ssh.Certificate)
	if !ok {
		return nil, trace.BadParameter("not a certificate")
	}

	return cert, nil
}

// SessionContext holds session specific context, such as SSH auth agents, PTYs,
// and other resources. SessionContext also holds a ServerContext which can be
// used to access resources on the underlying server. SessionContext can also
// be used to attach resources that should be closed once the session closes.
type ServerContext struct {
	*log.Entry

	sync.RWMutex

	// env is a list of environment variables passed to the session.
	env map[string]string

	// srv is the server that is holding the context.
	srv Server

	// id is the server specific incremental session id.
	id int

	// term holds PTY if it was requested by the session.
	term Terminal

	// agent is a client to remote SSH agent.
	agent agent.Agent

	// agentCh is SSH channel using SSH agent protocol.
	agentChannel ssh.Channel

	// session holds the active session (if there's an active one).
	session *session

	// closers is a list of io.Closer that will be called when session closes
	// this is handy as sometimes client closes session, in this case resources
	// will be properly closed and deallocated, otherwise they could be kept hanging.
	closers []io.Closer

	// Conn is the underlying *ssh.ServerConn.
	Conn *ssh.ServerConn

	// StatConn is the underlying net.Conn wrapped in a wrapper that tracks how
	// much data was transmitted.
	StatConn *utils.StatConn

	// Identity holds the identity of the user that is currently logged in on
	// the Conn.
	Identity IdentityContext

	// ExecResultCh is a Go channel which will be used to send and receive the
	// result of a "exec" request.
	ExecResultCh chan ExecResult

	// SubsystemResultCh is a Go channel which will be used to send and receive
	// the result of a "subsystem" request.
	SubsystemResultCh chan SubsystemResult

	// IsTestStub is set to true by tests.
	IsTestStub bool

	// ExecRequest is the command to be executed within this session context.
	ExecRequest Exec

	// ClusterName is the name of the cluster current user is authenticated with.
	ClusterName string

	// ClusterConfig holds the cluster configuration at the time this context was
	// created.
	ClusterConfig services.ClusterConfig

	// RemoteClient holds a SSH client to a remote server. Only used by the
	// recording proxy.
	RemoteClient *ssh.Client

	// RemoteSession holds a SSH session to a remote server. Only used by the
	// recording proxy.
	RemoteSession *ssh.Session

	// clientLastActive records the last time there was activity from the client
	clientLastActive time.Time

	// disconnectExpiredCert is set to time when/if the certificate should
	// be disconnected, set to empty if no disconect is necessary
	disconnectExpiredCert time.Time

	// clientIdleTimeout is set to the timeout on
	// on client inactivity, set to 0 if not setup
	clientIdleTimeout time.Duration

	// cancelContext signals closure to all outstanding operations
	cancelContext context.Context

	// cancel is called whenever server context is closed
	cancel context.CancelFunc
}

// NewServerContext creates a new *ServerContext which is used to pass and
// manage resources.
func NewServerContext(srv Server, conn *ssh.ServerConn, identityContext IdentityContext) (*ServerContext, error) {
	clusterConfig, err := srv.GetAccessPoint().GetClusterConfig()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	cancelContext, cancel := context.WithCancel(context.TODO())

	ctx := &ServerContext{
		id:                int(atomic.AddInt32(&ctxID, int32(1))),
		env:               make(map[string]string),
		srv:               srv,
		Conn:              conn,
		ExecResultCh:      make(chan ExecResult, 10),
		SubsystemResultCh: make(chan SubsystemResult, 10),
		ClusterName:       conn.Permissions.Extensions[utils.CertTeleportClusterName],
		ClusterConfig:     clusterConfig,
		Identity:          identityContext,
		clientIdleTimeout: identityContext.RoleSet.AdjustClientIdleTimeout(clusterConfig.GetClientIdleTimeout()),
		cancelContext:     cancelContext,
		cancel:            cancel,
	}

	disconnectExpiredCert := identityContext.RoleSet.AdjustDisconnectExpiredCert(clusterConfig.GetDisconnectExpiredCert())
	if !identityContext.CertValidBefore.IsZero() && disconnectExpiredCert {
		ctx.disconnectExpiredCert = identityContext.CertValidBefore
	}

	fields := log.Fields{
		"local":        conn.LocalAddr(),
		"remote":       conn.RemoteAddr(),
		"login":        ctx.Identity.Login,
		"teleportUser": ctx.Identity.TeleportUser,
		"id":           ctx.id,
	}
	if !ctx.disconnectExpiredCert.IsZero() {
		fields["cert"] = ctx.disconnectExpiredCert
	}
	if ctx.clientIdleTimeout != 0 {
		fields["idle"] = ctx.clientIdleTimeout
	}
	ctx.Entry = log.WithFields(log.Fields{
		trace.Component:       srv.Component(),
		trace.ComponentFields: fields,
	})

	if !ctx.disconnectExpiredCert.IsZero() || ctx.clientIdleTimeout != 0 {
		go ctx.periodicCheckDisconnect()
	}

	return ctx, nil
}

func (c *ServerContext) ID() int {
	return c.id
}

// SessionID returns the ID of the session in the context.
func (c *ServerContext) SessionID() rsession.ID {
	return c.session.id
}

// GetServer returns the underlying server which this context was created in.
func (c *ServerContext) GetServer() Server {
	return c.srv
}

// CreateOrJoinSession will look in the SessionRegistry for the session ID. If
// no session is found, a new one is created. If one is found, it is returned.
func (c *ServerContext) CreateOrJoinSession(reg *SessionRegistry) error {
	// As SSH conversation progresses, at some point a session will be created and
	// its ID will be added to the environment
	ssid, found := c.GetEnv(sshutils.SessionEnvVar)
	if !found {
		return nil
	}
	// make sure whatever session is requested is a valid session
	_, err := rsession.ParseID(ssid)
	if err != nil {
		return trace.BadParameter("invalid session id")
	}

	findSession := func() (*session, bool) {
		reg.Lock()
		defer reg.Unlock()
		return reg.findSession(rsession.ID(ssid))
	}

	// update ctx with a session ID
	c.session, _ = findSession()
	if c.session == nil {
		log.Debugf("Will create new session for SSH connection %v.", c.Conn.RemoteAddr())
	} else {
		log.Debugf("Will join session %v for SSH connection %v.", c.session, c.Conn.RemoteAddr())
	}

	return nil
}

func (c *ServerContext) periodicCheckDisconnect() {
	var certTime <-chan time.Time
	if !c.disconnectExpiredCert.IsZero() {
		t := time.NewTimer(c.disconnectExpiredCert.Sub(c.srv.GetClock().Now().UTC()))
		defer t.Stop()
		certTime = t.C
	}

	var idleTimer *time.Timer
	var idleTime <-chan time.Time
	if c.clientIdleTimeout != 0 {
		idleTimer = time.NewTimer(c.clientIdleTimeout)
		idleTime = idleTimer.C
	}

	for {
		select {
		// certificate has expired, disconnect
		case <-certTime:
			event := events.EventFields{
				events.EventType:       events.ClientDisconnectEvent,
				events.EventLogin:      c.Identity.Login,
				events.EventUser:       c.Identity.TeleportUser,
				events.LocalAddr:       c.Conn.LocalAddr().String(),
				events.RemoteAddr:      c.Conn.RemoteAddr().String(),
				events.SessionServerID: c.srv.ID(),
				events.Reason:          fmt.Sprintf("client certificate expired at %v", c.clientLastActive),
			}
			c.srv.EmitAuditEvent(events.ClientDisconnectEvent, event)
			c.Debugf("Disconnecting client: %v", event[events.Reason])
			c.Conn.Close()
			return
		case <-idleTime:
			now := c.srv.GetClock().Now()
			clientLastActive := c.GetClientLastActive()
			c.Debugf("client last active %v, client idle timeout %v", clientLastActive, c.clientIdleTimeout)
			if now.Sub(clientLastActive) >= c.clientIdleTimeout {
				event := events.EventFields{
					events.EventLogin:      c.Identity.Login,
					events.EventUser:       c.Identity.TeleportUser,
					events.LocalAddr:       c.Conn.LocalAddr().String(),
					events.RemoteAddr:      c.Conn.RemoteAddr().String(),
					events.SessionServerID: c.srv.ID(),
				}
				if clientLastActive.IsZero() {
					event[events.Reason] = "client reported no activity"
				} else {
					event[events.Reason] = fmt.Sprintf("client is idle for %v, exceeded idle timeout of %v",
						now.Sub(clientLastActive), c.clientIdleTimeout)
				}
				c.Debugf("Disconnecting client: %v", event[events.Reason])
				c.srv.EmitAuditEvent(events.ClientDisconnectEvent, event)
				c.Conn.Close()
				return
			}
			c.Debugf("Next check in %v", c.clientIdleTimeout-now.Sub(clientLastActive))
			idleTimer = time.NewTimer(c.clientIdleTimeout - now.Sub(clientLastActive))
			idleTime = idleTimer.C
		case <-c.cancelContext.Done():
			c.Debugf("Releasing associated resources - context has been closed.")
			return
		}
	}
}

// GetClientLastActive returns time when client was last active
func (c *ServerContext) GetClientLastActive() time.Time {
	c.RLock()
	defer c.RUnlock()
	return c.clientLastActive
}

// UpdateClientActivity sets last recorded client activity associated with this context
// either channel or session
func (c *ServerContext) UpdateClientActivity() {
	c.Lock()
	defer c.Unlock()
	c.clientLastActive = c.srv.GetClock().Now().UTC()
}

// AddCloser adds any closer in ctx that will be called
// whenever server closes session channel
func (c *ServerContext) AddCloser(closer io.Closer) {
	c.Lock()
	defer c.Unlock()
	c.closers = append(c.closers, closer)
}

// GetAgent returns a agent.Agent which represents the capabilities of an SSH agent.
func (c *ServerContext) GetAgent() agent.Agent {
	c.RLock()
	defer c.RUnlock()
	return c.agent
}

// GetAgentChannel returns the channel over which communication with the agent occurs.
func (c *ServerContext) GetAgentChannel() ssh.Channel {
	c.RLock()
	defer c.RUnlock()
	return c.agentChannel
}

// SetAgent sets the agent and channel over which communication with the agent occurs.
func (c *ServerContext) SetAgent(a agent.Agent, channel ssh.Channel) {
	c.Lock()
	defer c.Unlock()
	if c.agentChannel != nil {
		c.Infof("closing previous agent channel")
		c.agentChannel.Close()
	}
	c.agentChannel = channel
	c.agent = a
}

// GetTerm returns a Terminal.
func (c *ServerContext) GetTerm() Terminal {
	c.RLock()
	defer c.RUnlock()

	return c.term
}

// SetTerm set a Terminal.
func (c *ServerContext) SetTerm(t Terminal) {
	c.Lock()
	defer c.Unlock()

	c.term = t
}

// SetEnv sets a environment variable within this context.
func (c *ServerContext) SetEnv(key, val string) {
	c.env[key] = val
}

// GetEnv returns a environment variable within this context.
func (c *ServerContext) GetEnv(key string) (string, bool) {
	val, ok := c.env[key]
	return val, ok
}

// takeClosers returns all resources that should be closed and sets the properties to null
// we do this to avoid calling Close() under lock to avoid potential deadlocks
func (c *ServerContext) takeClosers() []io.Closer {
	// this is done to avoid any operation holding the lock for too long
	c.Lock()
	defer c.Unlock()

	closers := []io.Closer{}
	if c.term != nil {
		closers = append(closers, c.term)
		c.term = nil
	}
	if c.agentChannel != nil {
		closers = append(closers, c.agentChannel)
		c.agentChannel = nil
	}
	closers = append(closers, c.closers...)
	c.closers = nil
	return closers
}

func (c *ServerContext) Close() error {
	// When the context (connection) is closed, emit "session.data" event
	// containing how much data was transmitted and received.
	defer func() {
		clusterConfig, err := c.GetServer().GetAccessPoint().GetClusterConfig()
		if err != nil {
			log.Warnf("Failed to get: %v.", err)
			return
		}
		if clusterConfig.GetSessionRecording() == services.RecordAtProxy && c.GetServer().Component() != teleport.ComponentForwardingNode {
			return
		}

		fmt.Printf("--> c.GetServer().Component(): %v.\n", c.GetServer().Component())
		fmt.Printf("--> c.StatConn: %v.\n", c.StatConn)
		//	debug.PrintStack()

		auditLog := c.GetServer().GetAuditLog()
		if c.StatConn == nil {
			fmt.Printf("--> IS NIL\n")
			return
		}
		txBytes, rxBytes := c.StatConn.Stat()

		eventFields := events.EventFields{
			events.DataTransmitted: txBytes,
			events.DataReceived:    rxBytes,
			events.SessionServerID: c.GetServer().ID(),
			events.EventLogin:      c.Identity.Login,
			events.EventUser:       c.Identity.TeleportUser,
			events.LocalAddr:       c.Conn.LocalAddr().String(),
			events.RemoteAddr:      c.Conn.RemoteAddr().String(),
		}
		if c.session != nil {
			eventFields[events.SessionEventID] = c.session.id
		}

		auditLog.EmitAuditEvent(events.SessionDataEvent, eventFields)
	}()

	// Unblock any goroutines waiting until session is closed.
	c.cancel()

	// Close and release all resources.
	err := closeAll(c.takeClosers()...)
	if err != nil {
		return trace.Wrap(err)
	}

	return nil
}

// SendExecResult sends the result of execution of the "exec" command over the
// ExecResultCh.
func (c *ServerContext) SendExecResult(r ExecResult) {
	select {
	case c.ExecResultCh <- r:
	default:
		log.Infof("blocked on sending exec result %v", r)
	}
}

// SendSubsystemResult sends the result of running the subsystem over the
// SubsystemResultCh.
func (c *ServerContext) SendSubsystemResult(r SubsystemResult) {
	select {
	case c.SubsystemResultCh <- r:
	default:
		c.Infof("blocked on sending subsystem result")
	}
}

// ProxyPublicAddress tries to get the public address from the first
// available proxy. if public_address is not set, fall back to the hostname
// of the first proxy we get back.
func (c *ServerContext) ProxyPublicAddress() string {
	proxyHost := "<proxyhost>:3080"

	if c.srv == nil {
		return proxyHost
	}

	proxies, err := c.srv.GetAccessPoint().GetProxies()
	if err != nil {
		c.Errorf("Unable to retrieve proxy list: %v", err)
	}

	if len(proxies) > 0 {
		proxyHost = proxies[0].GetPublicAddr()
		if proxyHost == "" {
			proxyHost = fmt.Sprintf("%v:%v", proxies[0].GetHostname(), defaults.HTTPListenPort)
			c.Debugf("public_address not set for proxy, returning proxyHost: %q", proxyHost)
		}
	}

	return proxyHost
}

func (c *ServerContext) String() string {
	return fmt.Sprintf("ServerContext(%v->%v, user=%v, id=%v)", c.Conn.RemoteAddr(), c.Conn.LocalAddr(), c.Conn.User(), c.id)
}

func closeAll(closers ...io.Closer) error {
	var errs []error

	for _, cl := range closers {
		if cl == nil {
			continue
		}

		err := cl.Close()
		if err == nil {
			continue
		}

		errs = append(errs, err)
	}

	return trace.NewAggregate(errs...)
}

type closerFunc func() error

func (f closerFunc) Close() error {
	return f()
}

// NewTrackingReader returns a new instance of
// activity tracking reader.
func NewTrackingReader(ctx *ServerContext, r io.Reader) *TrackingReader {
	return &TrackingReader{ctx: ctx, r: r}
}

// TrackingReader wraps the writer
// and every time write occurs, updates
// the activity in the server context
type TrackingReader struct {
	ctx *ServerContext
	r   io.Reader
}

// Read passes the read through to internal
// reader, and updates activity of the server context
func (a *TrackingReader) Read(b []byte) (int, error) {
	a.ctx.UpdateClientActivity()
	return a.r.Read(b)
}
