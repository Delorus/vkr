package gateway

// ===============================================
// =========== Naucore handlers ==================
// ===============================================

type MsgHandler func(*Gateway, *Msg)

func dispatch(msg *Msg) MsgHandler {
	switch msg.Subject {
	case Subject{"Echo", Request}:
		return handleEchoRequest

	case Subject{"RegisterVirtualPeer", Response}:
		return handleRegisterVirtualPeerResponse

	default:
		switch msg.Subject.Type {
		case Request:
			return handleRequest
		case Response:
			return handleResponse
		case Command:
			return handleCommand
		case Event:
			return handleEvent
		default:
			return handleUnknownMsg
		}
	}
}

func handleEchoRequest(gw *Gateway, msg *Msg) {
	if err := gw.Bus.Send(&Msg{
		Proto:   NCCN,
		Subject: Subject{msg.Subject.Name, Response},
	}); err != nil {
		log.Errorln("Send echo response failed:", err)
	}
}

func handleRegisterVirtualPeerResponse(gw *Gateway, msg *Msg) {
	gw.registerPeerMu.Lock()
	login, ok := gw.tmpRegisteringPeers[msg.ID]
	if !ok {
		gw.registerPeerMu.Unlock()
		log.Errorln("Unidentified response RegisterVirtualPeer, id =", msg.ID)
		return
	}
	gw.registerPeerMu.Unlock()

	err := gw.Nats.Publish(msg.ID, []byte(login+"@"+gw.Bus.FullName()))
	if err != nil {
		log.Errorln("Cannot publish response for register virtual peer msg:", err)
		return
	}

	log.Infoln("Register new peer, with login =", login)
}

// handleRequest обрабатывает запросы с naucore.
// Что бы гарантировать получение ответа от клиента используется механизм запросов из NATS'а:
// отправляем сообщение на натс с сгенерированным ReplyID.
// На этот ID клиент должен выслать ответное сообщение, иначе, после того как истечет timeout, мы получим ошибку.
// После получения ответного сообщения от клиента, подменяем в нем теги маршрутизации naucore и отправляем обратно.
// Благодаря этому нам не нужно заниматься маршрутизацией на стороне клиента и мы можем использовать все преимущество натса.
func handleRequest(gw *Gateway, msg *Msg) {
	response, err := gw.Nats.Request(msg.Subject.Name, msg.Data, nats.DefaultTimeout)
	if err != nil {
		log.Errorf("Request for %s failed: %s", msg, err)
		return
	}

	log.Debugln("Response received, id =", msg.ID)
	err = gw.Bus.Send(&Msg{
		Proto:   NCC,
		Subject: Subject{msg.Subject.Name, Response},
		// Меняем местами, т.к. отправляем ответ на запрос
		From: msg.To,
		To:   msg.From,
		ID:   msg.ID,
		Data: response.Data,
	})
	if err != nil {
		log.Errorf("Send response with id '%s' to naucore failed: %s", msg.ID, err)
	}
}

// handleResponse принимает ответные сообщения с naucore.
// Что бы ответные сообщения дошли до клиентов NATS'а и они могли пользоваться стандартным механизмом,
// нужно заменить тему письма на его ID.
// Если по какой-то причине ID нет в письме, то отправляем как есть.
// В этом случае клиент не сможет пользоваться реквестом NATSа.
// Он должен будет самостоятельно подписаться на тему письма.
func handleResponse(gw *Gateway, msg *Msg) {
	natsMsg := &nats.Msg{Data: msg.Data}

	if msg.ID != Empty {
		natsMsg.Subject = msg.ID
	} else {
		log.Warnln("Response ID not found, forward to msg subject:", msg.Subject)
		natsMsg.Subject = msg.Subject.Name
	}

	if err := gw.Nats.PublishMsg(natsMsg); err != nil {
		log.Errorln("Cannot publish response:", err)
	}
}

func handleCommand(gw *Gateway, msg *Msg) {
	natsMsg := &nats.Msg{
		Subject: msg.Subject.Name,
		Data:    msg.Data,
	}

	if err := gw.Nats.PublishMsg(natsMsg); err != nil {
		log.WithError(err).Errorln("Cannot forward command:", msg)
	}
}

func handleEvent(gw *Gateway, msg *Msg) {
	natsMsg := &nats.Msg{
		Data: msg.Data,
	}

	// Если в команде типа Event есть ID,
	// значит оно было отправлено на Request от клиента NATSа,
	// в этом случае клиент будет ожидать ответные сообщения на ReplyID
	if msg.ID != Empty {
		natsMsg.Subject = msg.ID
	} else {
		natsMsg.Subject = msg.Subject.Name
	}

	if err := gw.Nats.PublishMsg(natsMsg); err != nil {
		log.WithError(err).Errorln("Cannot forward event:", msg)
	}
}

func handleUnknownMsg(gw *Gateway, msg *Msg) {
	// Отправляем как есть, без преобразований.
	// С такими сообщениями не работает натсовский механизм реквеста,
	// т.к. мы не можем добавить ReplyID из сообщения NATSа в реквест на naucore
	natsMsg := &nats.Msg{
		Subject: msg.Subject.Name,
		Data:    msg.Data,
	}

	if err := gw.Nats.PublishMsg(natsMsg); err != nil {
		log.WithError(err).Errorln("Cannot forward:", msg)
	}
}

func (gw *Gateway) SubscribeToCommonGroup() error {
	if err := gw.Bus.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}

	newSubscribeMsg := func(group string) (*Msg, error) {
		subscribe := new(cmd.SubscribeToGroupCommand)
		subscribe.SetPeer(gw.Bus.peer)
		subscribe.SetGroup(group)
		data, err := subscribe.Encode()
		if err != nil {
			return nil, err
		}
		return &Msg{
			Proto:   NCCN,
			Subject: Subject{"SubscribeToGroup", Command},
			Data:    data,
		}, nil
	}

	sendSubscribe := func(group string) error {
		msg, err := newSubscribeMsg(group)
		if err != nil {
			return fmt.Errorf("cannot create subscribe to %s: %s", group, err)
		}
		if err := gw.Bus.Send(msg); err != nil {
			return fmt.Errorf("cannot send subscribe to %s: %s", group, err)
		}
		return nil
	}

	for _, group := range [...]string{
		"_interface_registered",
		"_interface_unregistered",
		"_domain_registered",
		"_domain_unregistered",
		"_node_registered",
		"_node_unregistered",
		"_service_registered",
		"_service_unregistered",
		"_client_registered",
		"_client_unregistered",
	} {
		if err := sendSubscribe(group); err != nil {
			return err
		}
	}

	return gw.Bus.SetDeadline(time.Time{})
}

// ===============================================
// =========== NATS handlers =====================
// ===============================================

func (gw *Gateway) RegisterNatsSubscribe() {
	handleSubsErr := func(sub *nats.Subscription, err error) *nats.Subscription {
		if err != nil {
			panic(err)
		}
		return sub
	}

	handleSubsErr(gw.Nats.Subscribe(DefaultSubj+Request+".*", newNatsRequestHandler(gw)))
	handleSubsErr(gw.Nats.Subscribe(DefaultSubj+Command+".*", newNatsCommandHandler(gw)))
	handleSubsErr(gw.Nats.Subscribe(DefaultSubj+Event+".*", newNatsEventHandler(gw)))
	handleSubsErr(gw.Nats.Subscribe(DefaultSubj+Unknown+".*", newNatsUnknownHandler(gw)))
	handleSubsErr(gw.Nats.Subscribe(DefaultSubj+"RegisterVirtualPeer", newRegisterVirtualPeerRequestHandler(gw)))
	handleSubsErr(gw.Nats.Subscribe(DefaultSubj+"RegisterInterface", newSystemRequestHandler(gw)))
	handleSubsErr(gw.Nats.Subscribe(DefaultSubj+"ListInterfaces", newSystemRequestHandler(gw)))

	handleSubsErr(gw.Nats.Subscribe("GatewayAvailable", newGatewayAvailableHandler(gw)))
	go func() {
		for {
			if err := gw.connControl.WaitClosed(); err != nil {
				return
			}

			if err := gw.Nats.Publish("GatewayDisable", []byte(gw.ID)); err != nil {
				log.Errorf("Failed to publish \"gateway disable\" msg: %s", err)
			}

			if err := gw.connControl.WaitReopened(); err != nil {
				return
			}
		}
	}()
}

func newGatewayAvailableHandler(gw *Gateway) nats.MsgHandler {
	return func(msg *nats.Msg) {
		if msg.Reply == Empty || gw.connControl.Status() != OPEN {
			return
		}

		if err := gw.Nats.Publish(msg.Reply, []byte(gw.ID)); err != nil {
			log.WithError(err).Errorln("Cannot send gateway id")
		} else {
			log.Debugln("Send gateway id =", gw.ID)
		}
	}
}

func newNatsRequestHandler(gw *Gateway) nats.MsgHandler {
	return func(msg *nats.Msg) {
		if err := gw.Bus.Send(&Msg{
			Proto:   NCC,
			Subject: Subject{extractMsgSubject(msg.Subject), Request},
			ID:      msg.Reply,
			Data:    msg.Data,
		}); err != nil {
			log.Errorln("Send request to naucore failed:", err)
		}
	}
}

func newNatsCommandHandler(gw *Gateway) nats.MsgHandler {
	return func(msg *nats.Msg) {
		if err := gw.Bus.Send(&Msg{
			Proto:   NCC,
			Subject: Subject{extractMsgSubject(msg.Subject), Command},
			Data:    msg.Data,
		}); err != nil {
			log.Errorln("Send command to naucore failed:", err)
		}
	}
}

func newNatsEventHandler(gw *Gateway) nats.MsgHandler {
	return func(msg *nats.Msg) {
		if err := gw.Bus.Send(&Msg{
			Proto:   NCC,
			Subject: Subject{extractMsgSubject(msg.Subject), Event},
			Data:    msg.Data,
		}); err != nil {
			log.Errorln("Send event to naucore failed:", err)
		}
	}
}
func newNatsUnknownHandler(gw *Gateway) nats.MsgHandler {
	return func(msg *nats.Msg) {
		subject := extractMsgSubject(msg.Subject)
		if err := gw.Bus.Send(&Msg{
			Proto:   NCC,
			Subject: Subject{subject, Unknown},
			ID:      msg.Reply,
			Data:    msg.Data,
		}); err != nil {
			log.Errorf("Send %s msg to naucore failed: %v\n", subject, err)
		}
	}
}

func newRegisterVirtualPeerRequestHandler(gw *Gateway) nats.MsgHandler {
	return func(msg *nats.Msg) {
		login := string(msg.Data)

		gw.registerPeerMu.Lock()
		gw.peersCount++
		id := gw.peersCount
		fullLogin := login + "#" + strconv.Itoa(id)
		gw.tmpRegisteringPeers[msg.Reply] = fullLogin
		gw.registerPeerMu.Unlock()

		registerPeer := new(cmd.RegisterVirtualPeerRequest)
		registerPeer.Params.ShortVpeer = fullLogin
		data, err := registerPeer.Encode()
		if err != nil {
			log.Errorln("Cannot marshal RegisterVirtualPeer command:", err)
			return
		}

		if err := gw.Bus.Send(&Msg{
			Proto:   NCCN,
			Subject: Subject{"RegisterVirtualPeer", Request},
			ID:      msg.Reply,
			Data:    data,
		}); err != nil {
			log.Errorln("Send RegisterVirtualPeer request failed:", err)
		}
	}
}

func newSystemRequestHandler(gw *Gateway) nats.MsgHandler {
	return func(msg *nats.Msg) {
		subject := extractMsgSubject(msg.Subject)
		if err := gw.Bus.Send(&Msg{
			Proto:   NCCN,
			Subject: Subject{subject, Request},
			ID:      msg.Reply,
			Data:    msg.Data,
		}); err != nil {
			log.Errorf("Send %s request failed: %s\n", subject, err)
		}
	}
}

func extractMsgSubject(fullSubj string) string {
	return fullSubj[strings.LastIndexByte(fullSubj, '.')+1:]
}
