// Package centrifuge is a real-time core for Centrifugo server.
package centrifuge

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/centrifugal/centrifuge/internal/proto"
	"github.com/centrifugal/centrifuge/internal/proto/apiproto"
	"github.com/centrifugal/centrifuge/internal/proto/controlproto"

	"github.com/nats-io/nuid"
	uuid "github.com/satori/go.uuid"
)

// Node is a heart of Centrifugo – it internally keeps and manages client
// connections, maintains information about other Centrifugo nodes, keeps
// some useful references to things like engine, metrics etc.
type Node struct {
	mu sync.RWMutex

	// unique id for this node.
	uid string

	// startedAt is unix time of node start.
	startedAt int64

	// config for node.
	config Config

	// hub to manage client connections.
	hub *Hub

	// engine - in memory or redis.
	engine Engine

	// nodes contains registry of known nodes.
	nodes *nodeRegistry

	// shutdown is a flag which is only true when node is going to shut down.
	shutdown bool

	// shutdownCh is a channel which is closed when node shutdown initiated.
	shutdownCh chan struct{}

	// messageEncoder is encoder to encode messages for engine.
	messageEncoder proto.MessageEncoder

	// messageEncoder is decoder to decode messages coming from engine.
	messageDecoder proto.MessageDecoder

	// controlEncoder is encoder to encode control messages for engine.
	controlEncoder controlproto.Encoder

	// controlDecoder is decoder to decode control messages coming from engine.
	controlDecoder controlproto.Decoder

	// mediator contains application event handlers.
	mediator *Mediator

	logger *logger
}

// New creates Node, the only required argument is config.
func New(c Config) *Node {
	uid := uuid.NewV4().String()

	n := &Node{
		uid:            uid,
		nodes:          newNodeRegistry(uid),
		config:         c,
		hub:            newHub(),
		startedAt:      time.Now().Unix(),
		shutdownCh:     make(chan struct{}),
		messageEncoder: proto.NewProtobufMessageEncoder(),
		messageDecoder: proto.NewProtobufMessageDecoder(),
		controlEncoder: controlproto.NewProtobufEncoder(),
		controlDecoder: controlproto.NewProtobufDecoder(),
		logger:         nil,
	}
	return n
}

// SetLogHandler ...
func (n *Node) SetLogHandler(level LogLevel, handler LogHandler) {
	n.logger = newLogger(level, handler)
}

// Config returns a copy of node Config.
func (n *Node) Config() Config {
	n.mu.RLock()
	c := n.config
	n.mu.RUnlock()
	return c
}

// SetMediator binds mediator to node.
func (n *Node) SetMediator(m *Mediator) {
	n.mediator = m
}

// Mediator binds config to node.
func (n *Node) Mediator() *Mediator {
	return n.mediator
}

// Hub returns node's Hub.
func (n *Node) Hub() *Hub {
	return n.hub
}

// Version returns version of node.
func (n *Node) Version() string {
	return n.Config().Version
}

// Reload node config.
func (n *Node) Reload(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.config = c
	return nil
}

// NotifyShutdown returns a channel which will be closed on node shutdown.
func (n *Node) NotifyShutdown() chan struct{} {
	return n.shutdownCh
}

// Run performs all startup actions. At moment must be called once on start
// after engine and structure set.
func (n *Node) Run(e Engine) error {
	n.mu.Lock()
	n.engine = e
	n.mu.Unlock()

	if err := n.engine.run(); err != nil {
		return err
	}

	err := n.pubNode()
	if err != nil {
		n.logger.log(newLogEntry(LogLevelError, "error publishing node control command", map[string]interface{}{"error": err.Error()}))
	}
	go n.sendNodePing()
	go n.cleanNodeInfo()
	go n.updateMetrics()

	return nil
}

// Shutdown sets shutdown flag and does various clean ups.
func (n *Node) Shutdown() error {
	n.mu.Lock()
	if n.shutdown {
		n.mu.Unlock()
		return nil
	}
	n.shutdown = true
	close(n.shutdownCh)
	n.mu.Unlock()
	return n.hub.shutdown()
}

func (n *Node) updateGauges() {
	numClientsGauge.Set(float64(n.hub.NumClients()))
	numUsersGauge.Set(float64(n.hub.NumUsers()))
	numChannelsGauge.Set(float64(n.hub.NumChannels()))
}

func (n *Node) updateMetrics() {
	for {
		select {
		case <-n.shutdownCh:
			return
		case <-time.After(10 * time.Second):
			n.updateGauges()
		}
	}
}

func (n *Node) sendNodePing() {
	for {
		select {
		case <-n.shutdownCh:
			return
		case <-time.After(nodeInfoPublishInterval):
			err := n.pubNode()
			if err != nil {
				n.logger.log(newLogEntry(LogLevelError, "error publishing node control command", map[string]interface{}{"error": err.Error()}))
			}
		}
	}
}

func (n *Node) cleanNodeInfo() {
	for {
		select {
		case <-n.shutdownCh:
			return
		case <-time.After(nodeInfoCleanInterval):
			n.mu.RLock()
			delay := nodeInfoMaxDelay
			n.mu.RUnlock()
			n.nodes.clean(delay)
		}
	}
}

// Channels returns list of all engines clients subscribed on all Centrifugo nodes.
func (n *Node) Channels() ([]string, error) {
	return n.engine.channels()
}

// Info returns aggregated stats from all Centrifugo nodes.
func (n *Node) Info() (*NodeInfo, error) {
	nodes := n.nodes.list()
	nodeResults := make([]*apiproto.NodeResult, len(nodes))
	for i, nd := range nodes {
		nodeResults[i] = &apiproto.NodeResult{
			UID:         nd.UID,
			Name:        nd.Name,
			NumClients:  nd.NumClients,
			NumUsers:    nd.NumUsers,
			NumChannels: nd.NumChannels,
			Uptime:      nd.Uptime,
		}
	}

	return &NodeInfo{
		Engine: n.engine.name(),
		Nodes:  nodeResults,
	}, nil
}

// handleControl handles messages from control channel - control messages used for internal
// communication between nodes to share state or proto.
func (n *Node) handleControl(cmd *controlproto.Command) error {
	messagesReceivedCount.WithLabelValues("control").Inc()

	if cmd.UID == n.uid {
		// Sent by this node.
		return nil
	}

	method := cmd.Method
	params := cmd.Params

	switch method {
	case controlproto.MethodTypeNode:
		cmd, err := n.controlDecoder.DecodeNode(params)
		if err != nil {
			n.logger.log(newLogEntry(LogLevelError, "error decoding node control params", map[string]interface{}{"error": err.Error()}))
			return err
		}
		return n.nodeCmd(cmd)
	case controlproto.MethodTypeUnsubscribe:
		cmd, err := n.controlDecoder.DecodeUnsubscribe(params)
		if err != nil {
			n.logger.log(newLogEntry(LogLevelError, "error decoding unsubscribe control params", map[string]interface{}{"error": err.Error()}))
			return err
		}
		return n.hub.unsubscribe(cmd.User, cmd.Channel)
	case controlproto.MethodTypeDisconnect:
		cmd, err := n.controlDecoder.DecodeDisconnect(params)
		if err != nil {
			n.logger.log(newLogEntry(LogLevelError, "error decoding disconnect control params", map[string]interface{}{"error": err.Error()}))
			return err
		}
		return n.hub.disconnect(cmd.User, false)
	default:
		n.logger.log(newLogEntry(LogLevelError, "unknown control message method", map[string]interface{}{"method": method}))
		return fmt.Errorf("control method not found: %d", method)
	}
}

// handleClientMessage ...
func (n *Node) handleClientMessage(message *proto.Message) error {
	switch message.Type {
	case proto.MessageTypePublication:
		publication, err := n.messageDecoder.DecodePublication(message.Data)
		if err != nil {
			return err
		}
		n.handlePublication(message.Channel, publication)
	case proto.MessageTypeJoin:
		join, err := n.messageDecoder.DecodeJoin(message.Data)
		if err != nil {
			return err
		}
		n.handleJoin(message.Channel, join)
	case proto.MessageTypeLeave:
		leave, err := n.messageDecoder.DecodeLeave(message.Data)
		if err != nil {
			return err
		}
		n.handleLeave(message.Channel, leave)
	default:
	}
	return nil
}

// handlePublication handles messages published by web application or client into channel.
// The goal of this method to deliver this message to all clients on this node subscribed
// on channel.
func (n *Node) handlePublication(ch string, publication *Publication) error {
	messagesReceivedCount.WithLabelValues("publication").Inc()
	numSubscribers := n.hub.NumSubscribers(ch)
	hasCurrentSubscribers := numSubscribers > 0
	if !hasCurrentSubscribers {
		return nil
	}
	return n.hub.broadcastPublication(ch, publication)
}

// handleJoin handles join messages.
func (n *Node) handleJoin(ch string, join *proto.Join) error {
	messagesReceivedCount.WithLabelValues("join").Inc()
	hasCurrentSubscribers := n.hub.NumSubscribers(ch) > 0
	if !hasCurrentSubscribers {
		return nil
	}
	return n.hub.broadcastJoin(ch, join)
}

// handleLeave handles leave messages.
func (n *Node) handleLeave(ch string, leave *proto.Leave) error {
	messagesReceivedCount.WithLabelValues("leave").Inc()
	hasCurrentSubscribers := n.hub.NumSubscribers(ch) > 0
	if !hasCurrentSubscribers {
		return nil
	}
	return n.hub.broadcastLeave(ch, leave)
}

func makeErrChan(err error) <-chan error {
	ret := make(chan error, 1)
	ret <- err
	return ret
}

// Publish sends a message to all clients subscribed on channel. All running nodes
// will receive it and will send it to all clients on node subscribed on channel.
func (n *Node) Publish(ch string, pub *Publication) error {
	return <-n.publish(ch, pub, nil)
}

func (n *Node) publish(ch string, pub *Publication, opts *ChannelOptions) <-chan error {
	if opts == nil {
		chOpts, ok := n.ChannelOpts(ch)
		if !ok {
			return makeErrChan(ErrNamespaceNotFound)
		}
		opts = &chOpts
	}

	messagesSentCount.WithLabelValues("publication").Inc()

	if pub.UID == "" {
		pub.UID = nuid.Next()
	}

	return n.engine.publish(ch, pub, opts)
}

// publishJoin allows to publish join message into channel when someone subscribes on it
// or leave message when someone unsubscribes from channel.
func (n *Node) publishJoin(ch string, join *proto.Join, opts *ChannelOptions) <-chan error {
	if opts == nil {
		chOpts, ok := n.ChannelOpts(ch)
		if !ok {
			return makeErrChan(ErrNamespaceNotFound)
		}
		opts = &chOpts
	}
	messagesSentCount.WithLabelValues("join").Inc()
	return n.engine.publishJoin(ch, join, opts)
}

// publishLeave allows to publish join message into channel when someone subscribes on it
// or leave message when someone unsubscribes from channel.
func (n *Node) publishLeave(ch string, leave *proto.Leave, opts *ChannelOptions) <-chan error {
	if opts == nil {
		chOpts, ok := n.ChannelOpts(ch)
		if !ok {
			return makeErrChan(ErrNamespaceNotFound)
		}
		opts = &chOpts
	}
	messagesSentCount.WithLabelValues("leave").Inc()
	return n.engine.publishLeave(ch, leave, opts)
}

// publishControl publishes message into control channel so all running
// nodes will receive and handle it.
func (n *Node) publishControl(msg *controlproto.Command) <-chan error {
	messagesSentCount.WithLabelValues("control").Inc()
	return n.engine.publishControl(msg)
}

// pubNode sends control message to all nodes - this message
// contains information about current node.
func (n *Node) pubNode() error {
	n.mu.RLock()
	node := &controlproto.Node{
		UID:         n.uid,
		Name:        n.config.Name,
		Version:     n.config.Version,
		NumClients:  uint32(n.hub.NumClients()),
		NumUsers:    uint32(n.hub.NumUsers()),
		NumChannels: uint32(n.hub.NumChannels()),
		Uptime:      uint32(time.Now().Unix() - n.startedAt),
	}
	n.mu.RUnlock()

	params, _ := n.controlEncoder.EncodeNode(node)

	cmd := &controlproto.Command{
		UID:    n.uid,
		Method: controlproto.MethodTypeNode,
		Params: params,
	}

	err := n.nodeCmd(node)
	if err != nil {
		n.logger.log(newLogEntry(LogLevelError, "error handling node command", map[string]interface{}{"error": err.Error()}))
	}

	return <-n.publishControl(cmd)
}

// pubUnsubscribe publishes unsubscribe control message to all nodes – so all
// nodes could unsubscribe user from channel.
func (n *Node) pubUnsubscribe(user string, ch string) error {

	// TODO: looks it's already ok - need to check.
	unsubscribe := &controlproto.Unsubscribe{
		User:    user,
		Channel: ch,
	}

	params, _ := n.controlEncoder.EncodeUnsubscribe(unsubscribe)

	cmd := &controlproto.Command{
		UID:    n.uid,
		Method: controlproto.MethodTypeUnsubscribe,
		Params: params,
	}

	return <-n.publishControl(cmd)
}

// pubDisconnect publishes disconnect control message to all nodes – so all
// nodes could disconnect user from Centrifugo.
func (n *Node) pubDisconnect(user string, reconnect bool) error {

	disconnect := &controlproto.Disconnect{
		User: user,
	}

	params, _ := n.controlEncoder.EncodeDisconnect(disconnect)

	cmd := &controlproto.Command{
		UID:    n.uid,
		Method: controlproto.MethodTypeDisconnect,
		Params: params,
	}

	return <-n.publishControl(cmd)
}

// addClient registers authenticated connection in clientConnectionHub
// this allows to make operations with user connection on demand.
func (n *Node) addClient(c *client) error {
	actionCount.WithLabelValues("add_client").Inc()
	return n.hub.add(c)
}

// removeClient removes client connection from connection registry.
func (n *Node) removeClient(c *client) error {
	actionCount.WithLabelValues("remove_client").Inc()
	return n.hub.remove(c)
}

// addSubscription registers subscription of connection on channel in both
// engine and clientSubscriptionHub.
func (n *Node) addSubscription(ch string, c *client) error {
	actionCount.WithLabelValues("add_subscription").Inc()
	first, err := n.hub.addSub(ch, c)
	if err != nil {
		return err
	}
	if first {
		return n.engine.subscribe(ch)
	}
	return nil
}

// removeSubscription removes subscription of connection on channel
// from both engine and clientSubscriptionHub.
func (n *Node) removeSubscription(ch string, c *client) error {
	actionCount.WithLabelValues("remove_subscription").Inc()
	empty, err := n.hub.removeSub(ch, c)
	if err != nil {
		return err
	}
	if empty {
		return n.engine.unsubscribe(ch)
	}
	return nil
}

// nodeCmd handles ping control command i.e. updates information about known nodes.
func (n *Node) nodeCmd(node *controlproto.Node) error {
	n.nodes.add(node)
	return nil
}

// Unsubscribe unsubscribes user from channel, if channel is equal to empty
// string then user will be unsubscribed from all channels.
func (n *Node) Unsubscribe(user string, ch string) error {

	if string(user) == "" {
		return ErrBadRequest
	}

	if string(ch) != "" {
		_, ok := n.ChannelOpts(ch)
		if !ok {
			return ErrNamespaceNotFound
		}
	}

	// First unsubscribe on this node.
	err := n.hub.unsubscribe(user, ch)
	if err != nil {
		return ErrInternalServerError
	}
	// Second send unsubscribe control message to other nodes.
	err = n.pubUnsubscribe(user, ch)
	if err != nil {
		return ErrInternalServerError
	}
	return nil
}

// Disconnect allows to close all user connections to Centrifugo.
func (n *Node) Disconnect(user string, reconnect bool) error {

	if string(user) == "" {
		return ErrBadRequest
	}

	// first disconnect user from this node
	err := n.hub.disconnect(user, reconnect)
	if err != nil {
		return ErrInternalServerError
	}
	// second send disconnect control message to other nodes
	err = n.pubDisconnect(user, reconnect)
	if err != nil {
		return ErrInternalServerError
	}
	return nil
}

// namespaceName returns namespace name from channel if exists.
func (n *Node) namespaceName(ch string) string {
	cTrim := strings.TrimPrefix(ch, n.config.ChannelPrivatePrefix)
	if strings.Contains(cTrim, n.config.ChannelNamespaceBoundary) {
		parts := strings.SplitN(cTrim, n.config.ChannelNamespaceBoundary, 2)
		return parts[0]
	}
	return ""
}

// ChannelOpts returns channel options for channel using current channel config.
func (n *Node) ChannelOpts(ch string) (ChannelOptions, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.config.channelOpts(n.namespaceName(ch))
}

// addPresence proxies presence adding to engine.
func (n *Node) addPresence(ch string, uid string, info *proto.ClientInfo) error {
	n.mu.RLock()
	expire := n.config.ClientPresenceExpireInterval
	n.mu.RUnlock()
	actionCount.WithLabelValues("add_presence").Inc()
	return n.engine.addPresence(ch, uid, info, expire)
}

// removePresence proxies presence removing to engine.
func (n *Node) removePresence(ch string, uid string) error {
	actionCount.WithLabelValues("remove_presence").Inc()
	return n.engine.removePresence(ch, uid)
}

// Presence returns a map with information about active clients in channel.
func (n *Node) Presence(ch string) (map[string]*ClientInfo, error) {
	actionCount.WithLabelValues("presence").Inc()
	presence, err := n.engine.presence(ch)
	if err != nil {
		return nil, err
	}
	return presence, nil
}

// History returns a slice of last messages published into project channel.
func (n *Node) History(ch string) ([]*Publication, error) {
	actionCount.WithLabelValues("history").Inc()
	publications, err := n.engine.history(ch, historyFilter{Limit: 0})
	if err != nil {
		return nil, err
	}
	return publications, nil
}

// RemoveHistory removes channel history.
func (n *Node) RemoveHistory(ch string) error {
	actionCount.WithLabelValues("remove_history").Inc()
	return n.engine.removeHistory(ch)
}

// LastMessageID return last message id for channel.
func (n *Node) LastMessageID(ch string) (string, error) {
	actionCount.WithLabelValues("last_message_id").Inc()
	publications, err := n.engine.history(ch, historyFilter{Limit: 1})
	if err != nil {
		return "", err
	}
	if len(publications) == 0 {
		return "", nil
	}
	return publications[0].UID, nil
}

// privateChannel checks if channel private. In case of private channel
// subscription request must contain a proper signature.
func (n *Node) privateChannel(ch string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return strings.HasPrefix(string(ch), n.config.ChannelPrivatePrefix)
}

// userAllowed checks if user can subscribe on channel - as channel
// can contain special part in the end to indicate which users allowed
// to subscribe on it.
func (n *Node) userAllowed(ch string, user string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if !strings.Contains(ch, n.config.ChannelUserBoundary) {
		return true
	}
	parts := strings.Split(ch, n.config.ChannelUserBoundary)
	allowedUsers := strings.Split(parts[len(parts)-1], n.config.ChannelUserSeparator)
	for _, allowedUser := range allowedUsers {
		if string(user) == allowedUser {
			return true
		}
	}
	return false
}

// clientAllowed checks if client can subscribe on channel - as channel
// can contain special part in the end to indicate which client allowed
// to subscribe on it.
func (n *Node) clientAllowed(ch string, client string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if !strings.Contains(ch, n.config.ChannelClientBoundary) {
		return true
	}
	parts := strings.Split(ch, n.config.ChannelClientBoundary)
	allowedClient := parts[len(parts)-1]
	if string(client) == allowedClient {
		return true
	}
	return false
}

type nodeRegistry struct {
	// mu allows to synchronize access to node registry.
	mu sync.RWMutex
	// currentUID keeps uid of current node
	currentUID string
	// nodes is a map with information about known nodes.
	nodes map[string]controlproto.Node
	// updates track time we last received ping from node. Used to clean up nodes map.
	updates map[string]int64
}

func newNodeRegistry(currentUID string) *nodeRegistry {
	return &nodeRegistry{
		currentUID: currentUID,
		nodes:      make(map[string]controlproto.Node),
		updates:    make(map[string]int64),
	}
}

func (r *nodeRegistry) list() []controlproto.Node {
	r.mu.RLock()
	nodes := make([]controlproto.Node, len(r.nodes))
	i := 0
	for _, info := range r.nodes {
		nodes[i] = info
		i++
	}
	r.mu.RUnlock()
	return nodes
}

func (r *nodeRegistry) get(uid string) controlproto.Node {
	r.mu.RLock()
	info, _ := r.nodes[uid]
	r.mu.RUnlock()
	return info
}

func (r *nodeRegistry) add(info *controlproto.Node) {
	r.mu.Lock()
	r.nodes[info.UID] = *info
	r.updates[info.UID] = time.Now().Unix()
	r.mu.Unlock()
}

func (r *nodeRegistry) clean(delay time.Duration) {
	r.mu.Lock()
	for uid := range r.nodes {
		if uid == r.currentUID {
			// No need to clean info for current node.
			continue
		}
		updated, ok := r.updates[uid]
		if !ok {
			// As we do all operations with nodes under lock this should never happen.
			delete(r.nodes, uid)
			continue
		}
		if time.Now().Unix()-updated > int64(delay.Seconds()) {
			// Too many seconds since this node have been last seen - remove it from map.
			delete(r.nodes, uid)
			delete(r.updates, uid)
		}
	}
	r.mu.Unlock()
}
