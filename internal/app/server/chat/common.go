package chat

func (s *ChatSession) StopSpeaking(isSendTtsStop bool) {
	s.ClearChatTextQueue()
	s.llmManager.ClearLLMResponseQueue()
	s.ttsManager.ClearTTSQueue()

	s.clientState.CancelSessionCtx()

	if isSendTtsStop {
		s.serverTransport.SendTtsStop()
	}

	s.clientState.AudioState.SetPause()
}

func (s *ChatSession) MqttClose() {
	s.serverTransport.SendMqttGoodbye()
}
