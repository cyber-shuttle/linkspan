package controller

var ExternalShutdownChannel = make(chan string, 1)

func TriggerShutdown(reason string) {
	ExternalShutdownChannel <- reason
}
