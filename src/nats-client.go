package natsc

const ServiceName = "snitch-go"

const NauBuddyProtocolVersion = 700

// Надстройка над стандартным клиентом NATS, для удобного взаимодействия с шлюзом NATS<->Naucore.
// Определяет свои функции отправки сообщений в шину, которые адресуют все сообщения шлюзу.
// Так же умеет заново регистрировать все подписки и интерфейсы, если была потеряна связь со шлюзом.
type GatewayClient struct {
	conn      *nats.Conn
	GatewayID string

	enableWatchIfaceStatus bool
	interfaces             []*naucore.RegisterInterfaceRequest

	mu            sync.RWMutex
	iregistered   ifaceHandlers
	iunregistered ifaceHandlers
}

type (
	InterfaceHandler func(*naucore.PeerInterface)
	ifaceHandlers    map[string]InterfaceHandler
)

func ConnectToGateway(url string, opts ...nats.Option) (*GatewayClient, error) {
	conn, err := nats.Connect(url, opts...)
	if err != nil {
		return nil, fmt.Errorf("cannot connected to nats: %s", err)
	}
	gc := &GatewayClient{
		conn:          conn,
		iregistered:   make(ifaceHandlers),
		iunregistered: make(ifaceHandlers),
	}

	if _, err := gc.Subscribe("GatewayDisable", gc.gatewayDisableHandler); err != nil {
		return nil, err
	}

	if _, err := gc.Subscribe("InterfaceRegistered", func(msg *nats.Msg) {
		var event naucore.InterfaceRegisteredEvent
		if err := event.Decode(msg.Data); err != nil {
			log.Errorf("Cannot decode Event:InterfaceRegistered: %v", err)
			return
		}

		gc.mu.RLock()
		handler, exist := gc.iregistered[event.PeerInterface.Name()]
		gc.mu.RUnlock()

		if exist {
			handler(&event.PeerInterface)
		}
	}); err != nil {
		return nil, err
	}

	if _, err := gc.Subscribe("InterfaceUnregistered", func(msg *nats.Msg) {
		var event naucore.InterfaceUnregisteredEvent
		if err := event.Decode(msg.Data); err != nil {
			log.Errorf("Cannot decode Event:InterfaceUnregistered: %v", err)
			return
		}

		gc.mu.RLock()
		handler, exist := gc.iunregistered[event.PeerInterface.Name()]
		gc.mu.RUnlock()

		if exist {
			handler(&event.PeerInterface)
		}
	}); err != nil {
		return nil, err
	}

	return gc, gc.tryConnectToGateway()
}

func (gc *GatewayClient) tryConnectToGateway() error {
	for count := 0; count < gc.conn.Opts.MaxReconnect; count++ {
		msg, err := gc.conn.Request("GatewayAvailable", nil, gc.conn.Opts.Timeout)
		if err == nil {
			gc.GatewayID = string(msg.Data)
			log.Infoln("Connect to gateway =", gc.GatewayID)
			return nil
		}

		log.Warnln("Gateway not found:", err)
	}

	return fmt.Errorf("gateway not found")
}

func (gc *GatewayClient) RegisterPeer() error {
	_, err := gc.conn.Request("Gateway."+gc.GatewayID+".RegisterVirtualPeer", []byte(ServiceName), gc.conn.Opts.Timeout)
	return err
}

func (gc *GatewayClient) RegisterInterface(request *naucore.RegisterInterfaceRequest) error {
	data, err := request.Encode()
	if err != nil {
		return fmt.Errorf("cannot encode Request:RegisterInterface: %s", err)
	}

	// Не проверяем ответ т.к. в случае ошибки naucore просто разрывает соединение,
	// поэтому раз мы дошли до сюда, значит все отлично.
	_, err = gc.conn.Request("Gateway."+gc.GatewayID+".RegisterInterface", data, gc.conn.Opts.Timeout)
	if err != nil {
		return fmt.Errorf("register interface failed: %s", err)
	}

	gc.interfaces = append(gc.interfaces, request)

	return nil
}

func (gc *GatewayClient) ListInterfaces() (*naucore.ListInterfacesResponse, error) {
	msg, err := gc.conn.Request("Gateway."+gc.GatewayID+".ListInterfaces", nil, gc.conn.Opts.Timeout)
	if err != nil {
		return nil, fmt.Errorf("could not receive reply to ListInterfaces message: %s", err)
	}

	li := new(naucore.ListInterfacesResponse)
	return li, li.Decode(msg.Data)
}

func (gc *GatewayClient) EnableWatchInterfacesStatus() error {
	gc.enableWatchIfaceStatus = true
	resp, err := gc.ListInterfaces()
	if err != nil {
		return fmt.Errorf("failed get ListInterfaces: %v", err)
	}

	gc.mu.RLock()
	for i := range resp.Interfaces {
		if handler, exist := gc.iregistered[resp.Interfaces[i].Name()]; exist {
			go handler(&resp.Interfaces[i])
		}
	}
	gc.mu.RUnlock()

	return nil
}

func (gc *GatewayClient) OnInterfaceRegistered(iface string, handler InterfaceHandler) {
	gc.mu.Lock()
	gc.iregistered[iface] = handler
	gc.mu.Unlock()
}

func (gc *GatewayClient) OnInterfaceUnregistered(iface string, handler InterfaceHandler) {
	gc.mu.Lock()
	gc.iunregistered[iface] = handler
	gc.mu.Unlock()
}

func (gc *GatewayClient) Request(subj string, req c.EncodableMsg, resp c.DecodableMsg) error {
	return gc.RequestWithTimeout(subj, req, resp, gc.conn.Opts.Timeout)
}

func (gc *GatewayClient) RequestWithTimeout(subj string, req c.EncodableMsg, resp c.DecodableMsg, timeout time.Duration) error {
	if req == nil {
		return fmt.Errorf("request should not be nil")
	}

	data, err := req.Encode()
	if err != nil {
		return fmt.Errorf("cannot encode request %T: %v", req, err)
	}
	msg, err := gc.conn.Request("Gateway."+gc.GatewayID+".Request."+subj, data, timeout)
	if err != nil {
		return err
	}

	if resp != nil {
		if err := resp.Decode(msg.Data); err != nil {
			return fmt.Errorf("cannot decode response %T: %v", req, err)
		}
	}

	return nil
}

func (gc *GatewayClient) Response(replyTo *nats.Msg, resp c.EncodableMsg) error {
	if replyTo == nil {
		return fmt.Errorf("not found message to reply")
	}

	var data []byte
	if resp != nil {
		var err error
		if data, err = resp.Encode(); err != nil {
			return fmt.Errorf("cannot encode response %T: %v", resp, err)
		}
	}

	return gc.conn.Publish(replyTo.Reply, data)
}

func (gc *GatewayClient) Command(subj string, command c.EncodableMsg) error {
	if command == nil {
		return fmt.Errorf("command should not be nil")
	}

	data, err := command.Encode()
	if err != nil {
		return fmt.Errorf("cannot encode command %T: %v", command, err)
	}
	return gc.conn.Publish("Gateway."+gc.GatewayID+".Command."+subj, data)
}

func (gc *GatewayClient) Event(subj string, event c.EncodableMsg) error {
	if event == nil {
		return fmt.Errorf("event should not be nil")
	}

	data, err := event.Encode()
	if err != nil {
		return fmt.Errorf("cannot encode event %T: %v", event, err)
	}
	return gc.conn.Publish("Gateway."+gc.GatewayID+".Event."+subj, data)
}

func (gc *GatewayClient) PublishMsg(subj string, msg c.EncodableMsg) error {
	if msg == nil {
		return fmt.Errorf("msg should not be nil")
	}

	data, err := msg.Encode()
	if err != nil {
		return fmt.Errorf("cannot encode msg %T: %v", msg, err)
	}
	return gc.conn.Publish("Gateway."+gc.GatewayID+".Unknown."+subj, data)
}

func (gc *GatewayClient) Subscribe(subj string, cb nats.MsgHandler) (*nats.Subscription, error) {
	return gc.conn.Subscribe(subj, cb)
}

const (
	maxSyncMsg        = 1000
	warnBorderSyncMsg = 800
)

func (gc *GatewayClient) SubscribeSync(subj string, cb nats.MsgHandler) (*nats.Subscription, error) {
	ch := make(chan *nats.Msg, maxSyncMsg)
	sub, err := gc.conn.ChanSubscribe(subj, ch)
	if err != nil {
		return nil, err
	}
	go func() {
		for msg := range ch {
			if len(ch) >= warnBorderSyncMsg {
				log.Errorln("Too many task in sync queue")
			}
			cb(msg)
		}
	}()
	return sub, err
}

func (gc *GatewayClient) gatewayDisableHandler(msg *nats.Msg) {
	if string(msg.Data) != gc.GatewayID {
		return
	}

	log.Warnln("Gateway disabled")
	if err := gc.tryConnectToGateway(); err != nil {
		log.Fatalln("Gateway not found:", err)
	}

	if gc.enableWatchIfaceStatus {
		gc.EnableWatchInterfacesStatus()
	}

	interfaces := gc.interfaces
	gc.interfaces = gc.interfaces[:0]
	for _, interf := range interfaces {
		if err := gc.RegisterInterface(interf); err != nil {
			// Ошибка здесь означает какие-то проблемы с натсом,
			// возможно в этом случае стоит предпринять какие-то действия,
			// но пока не понятно какие, поэтому просто считаем, что ничего не работает.
			log.Errorf("Cannot re-register interface: %v", err)
			return
		}
	}
}

func (gc *GatewayClient) Close() error {
	gc.conn.Close()
	return nil
}

func (gc *GatewayClient) RegisterOnNauBuddy() (*naubuddy.RegisterResponse, error) {
	request := new(naubuddy.RegisterRequest)
	request.Params.ProtocolVersion = NauBuddyProtocolVersion

	resp := new(naubuddy.RegisterResponse)
	if err := gc.Request("Register", request, resp); err != nil {
		return nil, fmt.Errorf("registration on NauBuddy failed: %s", err)
	}

	if resp.Error != "" {
		return nil, fmt.Errorf("registration on NauBuddy failed: %s", resp.Error)
	}

	return resp, nil
}

func (gc *GatewayClient) RegisterSubscribeOnNauBuddy(request *naubuddy.SubscribeRequest) error {
	log.Infoln("Register subscribe:", request.Params.List)

	resp := new(naubuddy.SubscribeResponse)
	if err := gc.Request("Subscribe", request, resp); err != nil {
		return fmt.Errorf("subscribe failed: %s", err)
	}

	if resp.Error != "" {
		return fmt.Errorf("subscribe failed: %s", resp.Error)
	}

	return nil
}
