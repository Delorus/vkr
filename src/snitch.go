package main

type Snitch struct {
	startTime int64

	nats *natsc.GatewayClient

	projectModel   *models.ProjectModelRedis
	agentModel     *models.AgentModel
	callsProcessor *models.CallsProcessor
}

type SnitchOpts struct {
	CallProcessor models.CallProcessorOpts
	AgentModel    models.AgentModelOpts
}

func NewSnitch(nats *natsc.GatewayClient, redis *redis.Client, opts SnitchOpts) *Snitch {
	projectModel := models.NewProjectModel(redis)
	memStorage := models.CreateMemoryAgentModel()
	agentModel := models.NewAgentModelRedis(redis)
	return &Snitch{
		startTime:      time.Now().Unix(),
		nats:           nats,
		projectModel:   projectModel,
		agentModel:     models.CreateAgentModel(memStorage, agentModel, projectModel, opts.AgentModel),
		callsProcessor: models.NewCallProcessor(projectModel, memStorage, opts.CallProcessor),
	}
}

func (s *Snitch) RegisterInterfaces() error {
	s.nats.Subscribe("ModifyProjectThresholds", s.handleModifyProjectThresholds)
	s.nats.Subscribe("SubscribeToSLChange", s.handleSubscribeToSLChange)
	s.nats.Subscribe("UnsubscribeFromSLChange", s.handleUnsubscribeFromSLChange)

	ri := naucore.NewRegisterInterfaceRequest()
	ri.SetName("NauSnitch.0")
	ri.SetRange(naucore.Domain)
	ri.AddSignature("Request:ModifyProjectThresholds")
	ri.AddSignature("Request:SubscribeToSLChange")
	ri.AddSignature("Command:UnsubscribeFromSLChange")
	if err := s.nats.RegisterInterface(ri); err != nil {
		return err
	}

	return nil
}

func (s *Snitch) ListenBus() error {

	if err := s.handleQueueManager(); err != nil {
		log.Panicln("Subscribe to qpm failed:", err)
		return err
	}

	if err := s.handleNauBuddy(); err != nil {
		log.Panicln("Subscribe to buddy failed:", err)
		return err
	}

	if err := s.handleNauDialer(); err != nil {
		log.Panicln("Subscribe to dialer failed:", err)
		return err
	}

	s.nats.OnInterfaceRegistered("SnitchConfigurator.0", s.onSnitchConfiguratorRegistered)

	return s.nats.EnableWatchInterfacesStatus()
}

func (s *Snitch) handleQueueManager() error {
	if _, err := s.nats.Subscribe("FullProjectsConfig", s.handleFullProjectConfig); err != nil {
		return fmt.Errorf("cannot subscribe to FullProjectsConfig: %s", err)
	}

	if _, err := s.nats.Subscribe("ProjectsConfigChanged", s.handleProjectConfigChanged); err != nil {
		return err
	}

	handler, finish := s.buildOnQueueManagerRegisteredHandler()
	s.nats.OnInterfaceRegistered("QueueManager.0", handler)
	s.nats.OnInterfaceUnregistered("QueueManager.0", func(*naucore.PeerInterface) {
		finish()
	})

	return nil
}

func (s *Snitch) buildOnQueueManagerRegisteredHandler() (h natsc.InterfaceHandler, finish func()) {
	chFinish := make(chan struct{})

	handler := func(iface *naucore.PeerInterface) {
		log.Infoln("Subscribing to qpm")
		if err := s.nats.PublishMsg("SubscribeToFullProjectsConfig", new(nauqpm.SubscribeToProjectsConfig)); err != nil {
			log.Errorln("Subscribe to qpm failed:", err)
			return
		}
		go s.updateProjectParams(chFinish, 15*time.Second)
	}

	return handler, func() { chFinish <- struct{}{} }
}

func (s *Snitch) handleNauBuddy() error {
	if _, err := s.nats.Subscribe("FullBuddyList", s.handleFullBuddyList); err != nil {
		return fmt.Errorf("failed subscribe to FullBuddyList: %v", err)
	}
	if _, err := s.nats.Subscribe("ShortBuddyList", s.handleShortBuddyList); err != nil {
		return fmt.Errorf("failed subscribe to ShortBuddyList: %v", err)
	}
	if _, err := s.nats.Subscribe("BuddyListDiff", s.handleBuddyListDiff); err != nil {
		return fmt.Errorf("failed subscribe to BuddyListDiff: %v", err)
	}
	if _, err := s.nats.Subscribe("FullCallsList", s.handleCallsList); err != nil {
		return fmt.Errorf("failed subscribe to FullCallsList: %v", err)
	}
	if _, err := s.nats.SubscribeSync("ShortCallsList", s.handleCallsList); err != nil {
		return fmt.Errorf("failed subscribe to ShortCallsList: %v", err)
	}

	s.nats.OnInterfaceRegistered("NauBuddy.0", s.onNauBuddyRegistered)

	return nil
}

func (s *Snitch) onNauBuddyRegistered(iface *naucore.PeerInterface) {
	log.Infoln("Sending register request to naubuddy...")
	resp, err := s.nats.RegisterOnNauBuddy()
	if err != nil {
		log.Panicln("Buddy registration failed:", err)
		return
	}

	log.Infoln("Buddy registration succeed")
	if resp.Params.ProtocolVersion != natsc.NauBuddyProtocolVersion {
		log.Panicf("Expecting for protocol version %v, got %v", natsc.NauBuddyProtocolVersion, resp.Params.ProtocolVersion)
		return
	}

	log.Infoln("Sending function requests")

	buddylist := naubuddy.NewSubscribe(naubuddy.BuddyList)
	if err := s.nats.RegisterSubscribeOnNauBuddy(buddylist); err != nil {
		log.Panicln("Failed subscribe to naubuddy:", err)
		return
	}

	calllist := naubuddy.NewSubscribe(naubuddy.CallList)
	if err := s.nats.RegisterSubscribeOnNauBuddy(calllist); err != nil {
		log.Panicln("Failed subscribe to calllist:", err)
		return
	}
}

func (s *Snitch) onSnitchConfiguratorRegistered(iface *naucore.PeerInterface) {
	log.Debugln("Request project threshold on:", iface.Name())
	resp, err := s.sendRequestProjectThreshold()
	if err != nil {
		log.Errorln("Failed send Request:ProjectThreshold:", err)
		go s.updateProjectThreshold(30 * time.Second)
		return
	}

	if err := s.processProjectThreshold(resp.ProjectThresholds); err != nil {
		log.Errorln("Failed to process Request:ProjectThreshold:", err)
		go s.updateProjectThreshold(30 * time.Second)
		return
	}
}

func (s *Snitch) handleNauDialer() error {
	chFinish := make(chan struct{})
	handler := func(*naucore.PeerInterface) {
		s.updateCallListInfo(chFinish, 60*time.Second)
	}

	s.nats.OnInterfaceRegistered("NauDialer.0", handler)
	s.nats.OnInterfaceUnregistered("NauDialer.0", func(*naucore.PeerInterface) {
		chFinish <- struct{}{}
	})

	return nil
}

func (s *Snitch) handleFullProjectConfig(msg *nats.Msg) {
	projectConfig := new(nauqpm.FullProjectsConfigEvent)
	if err := projectConfig.Decode(msg.Data); err != nil {
		log.Errorln("Cannot decode FullProjectConfig:", err)
		return
	}

	log.Infoln("Processing full project config")
	projects := make([]string, 0, len(projectConfig.Projects))
	for i := range projectConfig.Projects {
		projects = append(projects, projectConfig.Projects[i].ID)
	}
	s.projectModel.AddProjects(projects...)
	s.projectModel.DeleteNonexistentProjects(projects...)
	log.Debugln("Processing full project config completed")
}

func (s *Snitch) handleProjectConfigChanged(msg *nats.Msg) {
	event := new(nauqpm.ProjectsConfigChangedEvent)
	if err := event.Decode(msg.Data); err != nil {
		log.Errorln("Cannot decode Event:ProjectsConfigChanged:", err)
		return
	}

	entry := log.WithField("id", event.ID)
	entry.Debugln("Processing changes to project config")
	newProjects := make([]string, 0, len(event.ProjectAdded))
	removeProjects := make([]string, 0, len(event.ProjectDeleted))
	currentProjects, err := s.projectModel.ProjectIDs()
	if err != nil {
		log.Errorln("Get current project ids failed:", err)
		return
	}
	currentProjectIDSet := make(utils.StringSet, len(currentProjects))
	for _, id := range currentProjects {
		currentProjectIDSet.Add(id)
	}

	for i := range event.ProjectAdded {
		if !currentProjectIDSet.Contains(event.ProjectAdded[i].ID) {
			newProjects = append(newProjects, event.ProjectAdded[i].ID)
		}
	}
	for i := range event.ProjectDeleted {
		if currentProjectIDSet.Contains(event.ProjectDeleted[i].ID) {
			removeProjects = append(removeProjects, event.ProjectDeleted[i].ID)
		}
	}

	s.projectModel.AddProjects(newProjects...)
	s.projectModel.DeleteProjectData(removeProjects...)
	entry.Debugln("Processing changes to project config completed")
}

func (s *Snitch) handleCallsList(msg *nats.Msg) {
	callList := new(naubuddy.FullCallsList)
	if err := callList.Decode(msg.Data); err != nil {
		log.Errorln("Cannot decode CallsList:", err)
		return
	}

	s.callsProcessor.HandleCallsList(callList)
}

func (s *Snitch) handleFullBuddyList(msg *nats.Msg) {
	buddyList := new(naubuddy.FullBuddyList)
	if err := buddyList.Decode(msg.Data); err != nil {
		log.Errorln("Cannot decode FullBuddyList:", err)
		return
	}

	s.agentModel.ProcessFullBuddyList(buddyList)
}

func (s *Snitch) handleShortBuddyList(msg *nats.Msg) {
	buddyList := new(naubuddy.ShortBuddyList)
	if err := buddyList.Decode(msg.Data); err != nil {
		log.Errorln("Cannot decode ShortBuddyList:", err)
		return
	}

	s.agentModel.ProcessShortBuddyList(buddyList)
}

func (s *Snitch) handleBuddyListDiff(msg *nats.Msg) {
	diff := new(naubuddy.BuddyListDiff)
	if err := diff.Decode(msg.Data); err != nil {
		log.Errorln("Cannot decode BuddyListDiff:", err)
		return
	}

	log.Debugln("Process BuddyListDiff")
	s.agentModel.ProcessBuddyListDiff(diff)
}

func (s *Snitch) handleModifyProjectThresholds(msg *nats.Msg) {
	req := new(nausnitch.ModifyProjectThresholdRequest)
	if err := req.Decode(msg.Data); err != nil {
		log.Errorln("Cannot decode Request:ModifyProjectThreshold:", err)
		return
	}
	entry := log.WithField("id", req.ID)
	entry.Debugln("Handle request to modify project threshold")
	if err := s.processProjectThreshold(req.ProjectThresholds()); err != nil {
		entry.Errorln("Cannot process project threshold:", err)
		return
	}
	if err := s.nats.Response(msg, nil); err != nil {
		entry.Errorln("Response to Request:ModifyProjectThresholds failed:", err)
		return
	}
	entry.Debugln("Modify project threshold completed")
}

func (s *Snitch) handleSubscribeToSLChange(msg *nats.Msg) {
	req := new(nausnitch.SubscribeToSLChangeRequest)
	resp := new(nausnitch.SubscribeToSLChangeResponse)
	if err := req.Decode(msg.Data); err != nil {
		log.Errorln("Cannot decode Request:SubscribeToSLChange:", err)
		resp.Error = "Invalid threshold value"
		s.nats.Response(msg, resp)
		return
	}

	entry := log.WithField("projectID", req.Params.ProjectID)
	entry.Debugln("Handle Request:SubscribeToSLChange")
	models.CallbackDataStore.Swap(req.Params.ProjectID, func(old *models.ProjectCallbackData) {
		old.ServiceLevelThreshold = req.Params.Threshold
		resp.Params.State = old.State()
	})

	if err := s.nats.Response(msg, resp); err != nil {
		entry.Errorln("Response to Request:SubscribeToSLChange failed:", err)
		return
	}
	entry.Debugln("Complete handle Request:SubscribeToSLChange")
}

func (s *Snitch) handleUnsubscribeFromSLChange(msg *nats.Msg) {
	command := new(nausnitch.UnsubscribeToSLChangeCommand)
	if err := command.Decode(msg.Data); err != nil {
		log.Errorln("Cannot decode Command:UnsubscribeToSLChange:", err)
		return
	}

	entry := log.WithField("projectID", command.Params.ProjectID)
	entry.Debugln("Handle Command:UnsubscribeToSLChange")
	models.CallbackDataStore.Delete(command.Params.ProjectID)
	entry.Debugln("Complete handle Command:UnsubscribeToSLChange")
}

func (s *Snitch) sendEventSLChange(projectID, state string) error {
	log.WithField("projectID", projectID).Infoln("Send Event:SLChange")
	event := new(nausnitch.SLChangeEvent)
	event.Params.ProjectID = projectID
	event.Params.State = state
	return s.nats.Event("SLChange", event)
}

func (s *Snitch) sendRequestProjectParam() (*nauqpm.ProjectsParamsResponse, error) {
	ids, err := s.projectModel.ProjectIDs()
	if err != nil {
		return nil, err
	}

	if len(ids) == 0 {
		log.Warnln("Not found projects for project params request")
		return &nauqpm.ProjectsParamsResponse{}, nil
	}

	request := new(nauqpm.ProjectsParamsRequest)
	request.AddProjectParam("waittime")
	for i := range ids {
		request.AddProject(ids[i])
	}

	resp := new(nauqpm.ProjectsParamsResponse)
	// тяжелый запрос, нужно больше время для ответа
	if err := s.nats.RequestWithTimeout("ProjectsParams", request, resp, 10*time.Second); err != nil {
		return nil, err
	}

	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}

	return resp, nil
}

func (s *Snitch) sendRequestProjectThreshold() (*naucrm.ProjectsThresholdsResponse, error) {
	resp := new(naucrm.ProjectsThresholdsResponse)

	if err := s.nats.Request("ProjectsThresholds", new(naucrm.ProjectsThresholdsRequest), resp); err != nil {
		return nil, err
	}

	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}

	return resp, nil
}

func (s *Snitch) UpdateServiceLevel(interval time.Duration) {
	ticker := time.NewTicker(interval)
	for range ticker.C {
		log.Debugln("Updating service level")
		// обновляем SL только для проектов, у которых есть порог для коллбэков, т.к. для другого SL сейчас не нужен
		for projectID, callbackData := range models.CallbackDataStore.CopyData() {
			if callbackData.ServiceLevelThreshold == 0 {
				continue
			}

			oldState := callbackData.State()
			//todo куча запросов к бд в цикле
			s.refreshServiceLevel(projectID, callbackData)
			state := callbackData.State()
			if state == oldState {
				continue
			}

			models.CallbackDataStore.Put(projectID, callbackData)

			log.WithField("projectID", projectID).
				Infof("Service level %v with threshold %v, callback state changed to %v",
					callbackData.ServiceLevel, callbackData.ServiceLevelThreshold, state)

			if err := s.sendEventSLChange(projectID, state); err != nil {
				log.WithField("projectID", projectID).Errorf("Failed to send Event:SLChange: %v", err)
			}
		}
		log.Debugln("Service level updated")
	}
}

func (s *Snitch) refreshServiceLevel(projectID string, callbackData *models.ProjectCallbackData) {
	entry := log.WithField("projectID", projectID)
	now := time.Now().Unix()
	if now-s.startTime < int64(models.SLPeriod.Seconds()) {
		entry.Debugln("Not enough data to calculate service level for project")
		return
	}

	currentProcessedBeforeThreshold := callbackData.CurrentProcessedBeforeThresholdCount()
	currentUnblocked := callbackData.CurrentUnblockedCount()

	dbProcessedBeforeThreshold, dbDenominator, err := db.GetServiceLevelData(projectID, models.SLPeriod)
	if err != nil {
		entry.Warnln(err)
		return
	}

	processedBeforeThreshold := dbProcessedBeforeThreshold + currentProcessedBeforeThreshold
	denominator := dbDenominator + currentUnblocked

	// мы считаем, что если звонков не было, то ServiceLevel "идеальный"
	if denominator == 0 {
		callbackData.ServiceLevel = 100
	} else {
		callbackData.ServiceLevel = 100 * processedBeforeThreshold / denominator
	}
	entry.Debugf("New service level is %d", callbackData.ServiceLevel)
}

func (s *Snitch) updateProjectParams(finish <-chan struct{}, interval time.Duration) {
	ticker := time.NewTicker(interval)
	for {
		select {
		case <-ticker.C:
			log.Infoln("Updating project params")
			projectParam, err := s.sendRequestProjectParam()
			if err != nil {
				log.Errorln("Failed send Request:ProjectParams:", err)
				continue
			}

			s.processProjectParams(projectParam.ProjectsParams)
			log.Debugln("Project params updated")
		case <-finish:
			log.Debugln("Updating project params finished")
			return
		}
	}
}

func (s *Snitch) processProjectParams(params []nauqpm.ProjectParams) {
	for i := range params {
		if params[i].Param.Contains("waittime") {
			waitTime := params[i].Param.Get("waittime")
			meanWait, err := strconv.Atoi(waitTime)
			if err != nil {
				log.WithField("projectID", params[i].ProjectUUID).
					Errorf("Incorrect waittime: '%v'", waitTime)
				continue
			}

			s.projectModel.SetProjectMeanWait(params[i].ProjectUUID, meanWait)
		}
	}
}

func (s *Snitch) updateProjectThreshold(interval time.Duration) {
	ticker := time.NewTicker(interval)
	for range ticker.C {
		log.Infoln("Attempt to update projects threshold")
		resp, err := s.sendRequestProjectThreshold()
		if err != nil {
			log.Errorln("Failed send Request:ProjectThreshold:", err)
			continue
		}

		if err := s.processProjectThreshold(resp.ProjectThresholds); err != nil {
			log.Errorln("Failed process project threshold:", err)
			continue
		}

		// успешно обработали сообщение
		log.Debugln("Projects threshold successful updated")
		return
	}
}

func (s *Snitch) processProjectThreshold(projectsThreshold []naucrm.ProjectThreshold) error {
	projectInfos := make(map[string]models.ProjectInfo, len(projectsThreshold))

	for i := range projectsThreshold {
		projectUUID := projectsThreshold[i].ProjectUUID

		projectInfos[projectUUID] = models.ProjectInfo{
			Threshold:         projectsThreshold[i].Threshold,
			TargetServiceTime: projectsThreshold[i].TargetServiceTime,
		}
	}

	return s.projectModel.SaveProjectsInfo(projectInfos)
}

func (s *Snitch) updateCallListInfo(finish <-chan struct{}, interval time.Duration) {
	ticker := time.NewTicker(interval)
	for {
		select {
		case <-ticker.C:
			log.Infoln("Updating call list info")
			resp, err := s.sendRequestCallListInfo()
			if err != nil {
				log.Errorln("Send CallsListsInfo request failed:", err)
				continue
			}

			for i := range resp.CallListInfo {
				projectUUID := resp.CallListInfo[i].ProjectUUID

				numbers, err := resp.CallListInfo[i].AllowedNumbers()
				if err != nil {
					log.WithField("projectID", projectUUID).Errorf("Incorrect allowed_numbers: %v", err)
					continue
				}

				//todo pipeline
				s.projectModel.SetCallListInfo(resp.CallListInfo[i].ProjectUUID, numbers, interval)
			}

			log.Debugln("Updating call list info completed")
		case <-finish:
			log.Debugln("Close update call list info")
			return
		}
	}
}

func (s *Snitch) sendRequestCallListInfo() (*naudialer.CallsListsInfoResponse, error) {
	req := new(naudialer.CallsListsInfoRequest)
	resp := new(naudialer.CallsListsInfoResponse)
	// тяжелый запрос, нужно больше время для ответа
	if err := s.nats.RequestWithTimeout("CallsListsInfo", req, resp, 10*time.Second); err != nil {
		return nil, err
	}

	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}

	return resp, nil
}

func (s *Snitch) SaveSummary(interval time.Duration) {
	ticker := time.NewTicker(interval)
	for range ticker.C {
		log.Infoln("Saving summary in db")
		ids, err := s.projectModel.ProjectIDs()
		if err != nil {
			log.Errorln("Save summary in db failed:", err)
			continue
		}

		if err := db.ExecuteInTx(func(tx *sqlx.Tx) error {
			for _, id := range ids {
				if err := models.CollectInboundProjectSummary(s.projectModel, id).InsertTx(tx); err != nil {
					return err
				}

				if err := models.CollectOutboundProjectSummary(s.projectModel, id).InsertTx(tx); err != nil {
					return err
				}
			}

			return models.CollectBlockedCalls(s.projectModel).InsertTx(tx)
		}); err != nil {
			log.Errorln("Save summary in db failed:", err)
			continue
		}
		log.Debugln("Saving summary in db completed")
	}
}

func (s *Snitch) CleanDB(interval time.Duration) {
	ticker := time.NewTicker(interval)
	for range ticker.C {
		log.Infoln("Database cleaning started")
		if err := db.DeleteOutdatedData(); err != nil {
			log.Errorln("Clean db failed:", err)
			continue
		}
		log.Debugln("Database cleaning completed")
	}
}
