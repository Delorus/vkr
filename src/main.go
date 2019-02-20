package main // import "repo.cc.naumen.ru/nauphone-snitch-go"

var NatsClient *natsc.GatewayClient

var v = "unknown"

func main() {
	if err := CreateConfig(); err != nil {
		panic(err)
	}

	ConfigLogging()

	go func() {
		port := viper.GetInt(DebugPort)
		if port == 0 {
			return
		}
		if err := http.ListenAndServe("localhost:"+strconv.Itoa(port), nil); err != nil {
			log.WithError(err).Warnf("Binding to debug port %d failed", port)
		}
	}()

	log.Infoln("Start NauSnitch, version:", v)

	ConfigDB()

	server := natsc.RunNatsBackground(ConfigNatsServer())
	defer server.Shutdown()

	busOpts := gateway.Options{
		URL:     viper.GetString(BusAddress),
		KeyFile: viper.GetString(KeyFile),
	}

	gw, err := gateway.RunGatewayBackground(busOpts, nats.DefaultURL)
	if err != nil {
		panic(err)
	}
	defer gw.Close()

	NatsClient, err = natsc.ConnectToGateway(nats.DefaultURL)
	if err != nil {
		panic(err)
	}
	defer NatsClient.Close()

	if err := NatsClient.RegisterPeer(); err != nil {
		panic(err)
	}

	redis := ConfigRedis()
	defer redis.Close()
	log.Infoln("Clean Redis database")
	redis.FlushDB()

	snitchConfig := ConfigSnitch()
	snitch := NewSnitch(NatsClient, redis, snitchConfig)
	if err := snitch.RegisterInterfaces(); err != nil {
		panic(err)
	}
	snitch.ListenBus()
	go snitch.UpdateServiceLevel(60 * time.Second)
	go snitch.SaveSummary(viper.GetDuration(DBCreateSummaryInterval))
	go snitch.CleanDB(viper.GetDuration(DBCleanDBInterval))

	waitSystemInterrupt()

	log.Infoln("======================================================= Exit =======================================================")
}

func waitSystemInterrupt() {
	chInterrupt := make(chan os.Signal, 1)
	signal.Notify(chInterrupt, os.Interrupt)

	<-chInterrupt
}
