package common

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	log "xiaozhi-esp32-server-golang/logger"

	"xiaozhi-esp32-server-golang/internal/app/server/auth"
	"xiaozhi-esp32-server-golang/internal/domain/llm"
	llm_common "xiaozhi-esp32-server-golang/internal/domain/llm/common"
	llm_memory "xiaozhi-esp32-server-golang/internal/domain/llm/memory"
	"xiaozhi-esp32-server-golang/internal/domain/vad"

	types_audio "xiaozhi-esp32-server-golang/internal/data/audio"
	. "xiaozhi-esp32-server-golang/internal/data/client"
	"xiaozhi-esp32-server-golang/internal/domain/audio"

	. "xiaozhi-esp32-server-golang/internal/data/msg"

	"github.com/gorilla/websocket"
	"github.com/spf13/viper"
)

// ServerMessage 表示服务器消息
type ServerMessage struct {
	Type        string                   `json:"type"`
	Text        string                   `json:"text,omitempty"`
	SessionID   string                   `json:"session_id,omitempty"`
	Version     int                      `json:"version"`
	State       string                   `json:"state,omitempty"`
	Transport   string                   `json:"transport,omitempty"`
	AudioFormat *types_audio.AudioFormat `json:"audio_params,omitempty"`
	Emotion     string                   `json:"emotion,omitempty"`
}

func HandleLLMResponse(ctx context.Context, state *ClientState, llmResponseChannel chan llm_common.LLMResponseStruct) (bool, error) {
	log.Debugf("HandleLLMResponse start")
	defer log.Debugf("HandleLLMResponse end")

	var fullText bytes.Buffer
	for {
		select {
		case llmResponse, ok := <-llmResponseChannel:
			if !ok {
				// 通道已关闭，退出协程
				log.Infof("LLM 响应通道已关闭，退出协程")
				return true, nil
			}

			log.Debugf("LLM 响应: %+v", llmResponse)

			// 使用带上下文的TTS处理
			outputChan, err := state.TTSProvider.TextToSpeechStream(state.Ctx, llmResponse.Text, state.OutputAudioFormat.SampleRate, state.OutputAudioFormat.Channels, state.OutputAudioFormat.FrameDuration)
			if err != nil {
				log.Errorf("生成 TTS 音频失败: %v", err)
				return true, fmt.Errorf("生成 TTS 音频失败: %v", err)
			}

			if llmResponse.IsStart {
				// 先发送文本
				response := ServerMessage{
					Type:      ServerMessageTypeTts,
					State:     MessageStateStart,
					SessionID: state.SessionID,
				}
				if err := state.SendMsg(response); err != nil {
					log.Errorf("发送 TTS Start 失败: %v", err)
					return true, fmt.Errorf("发送 TTS Start 失败: %v", err)
				}
			}

			// 先发送文本
			response := ServerMessage{
				Type:      ServerMessageTypeTts,
				State:     MessageStateSentenceStart,
				Text:      llmResponse.Text,
				SessionID: state.SessionID,
			}
			if err := state.SendMsg(response); err != nil {
				log.Errorf("发送 TTS 文本失败: %s, %v", llmResponse.Text, err)
				return true, fmt.Errorf("发送 TTS 文本失败: %s, %v", llmResponse.Text, err)
			}

			fullText.WriteString(llmResponse.Text)

			// 发送音频帧
			if err := state.SendTTSAudio(ctx, outputChan, llmResponse.IsStart); err != nil {
				log.Errorf("发送 TTS 音频失败: %s, %v", llmResponse.Text, err)
				return true, fmt.Errorf("发送 TTS 音频失败: %s, %v", llmResponse.Text, err)
			}

			// 先发送文本
			response = ServerMessage{
				Type:      ServerMessageTypeTts,
				State:     MessageStateSentenceEnd,
				Text:      llmResponse.Text,
				SessionID: state.SessionID,
			}
			if err := state.SendMsg(response); err != nil {
				log.Errorf("发送 TTS 文本失败: %s, %v", llmResponse.Text, err)
				return true, fmt.Errorf("发送 TTS 文本失败: %s, %v", llmResponse.Text, err)
			}

			if llmResponse.IsEnd {
				//延迟50ms毫秒再发stop
				//time.Sleep(50 * time.Millisecond)
				//写到redis中
				llm_memory.Get().AddMessage(ctx, state.DeviceID, "assistant", fullText.String())
				// 发送结束消息
				response := ServerMessage{
					Type:      ServerMessageTypeTts,
					State:     MessageStateStop,
					SessionID: state.SessionID,
				}
				if err := state.SendMsg(response); err != nil {
					log.Errorf("发送 TTS 文本失败: stop, %v", err)
					return false, fmt.Errorf("发送 TTS 文本失败: stop")
				}

				return ok, nil
			}
		case <-ctx.Done():
			// 上下文已取消，退出协程
			log.Infof("设备 %s 连接已关闭，停止处理LLM响应", state.DeviceID)
			return false, nil
		}
	}

}

func ProcessVadAudio(state *ClientState) {
	go func() {
		audioFormat := state.InputAudioFormat
		audioProcessor, err := audio.GetAudioProcesser(
			audioFormat.SampleRate,
			audioFormat.Channels,
			audioFormat.FrameDuration,
		)
		if err != nil {
			log.Errorf("获取解码器失败: %v", err)
			return
		}

		// 计算 VAD 帧大小（样本数）
		frameSize := (audioFormat.SampleRate * audioFormat.FrameDuration) / 1000
		frameBytes := frameSize * 2 // 16-bit = 2字节/样本

		// 创建 int16 缓冲区（足够容纳一帧数据）
		int16Buffer := make([]int16, frameSize*audioFormat.Channels)
		byteBuffer := make([]byte, 0, 2048) // 字节缓冲区

		// 计算需要多少帧进行 VAD 检测
		vadNeedGetCount := 60 / audioFormat.FrameDuration

		var skipVad bool
		for {
			select {
			case opusFrame, ok := <-state.OpusAudioBuffer:
				if state.GetClientVoiceStop() {
					continue
				}
				if !ok {
					log.Debugf("音频通道已关闭")
					return
				}

				clientHaveVoice := state.GetClientHaveVoice()
				var haveVoice bool
				if state.ListenMode != "auto" {
					haveVoice = true
					clientHaveVoice = true
					skipVad = true
				}

				// 直接解码为 int16
				n, err := audioProcessor.Decoder(opusFrame, int16Buffer)
				if err != nil {
					log.Errorf("解码失败: %v", err)
					continue
				}

				// 获取实际解码的样本
				decodedSamples := int16Buffer[:n]

				// 处理多声道：转换为单声道（VAD 需要单声道）
				var monoPCM []int16
				if audioFormat.Channels > 1 {
					monoPCM = convertToMono(decodedSamples, audioFormat.Channels)
				} else {
					monoPCM = decodedSamples
				}

				// 当检测到语音时，保存音频数据到缓冲区（ASR 需要 float32）
				if clientHaveVoice || haveVoice {
					floatData := int16ToFloat32(monoPCM)
					state.AudioBuffer = append(state.AudioBuffer, floatData...)
					log.Debugf("添加到音频缓冲区，当前长度: %d", len(state.AudioBuffer))
				}

				// 转换为字节数据 (小端序)
				byteData := int16ToBytes(monoPCM)
				byteBuffer = append(byteBuffer, byteData...)

				if !skipVad {
					// 初始化VAD（如果未初始化）
					if state.VadProvider == nil {
						if err := initVAD(state, audioFormat); err != nil {
							log.Errorf("初始化VAD失败: %v", err)
							continue
						}
					}

					// 添加到ASR缓冲区（转换为 float32）
					state.AsrAudioBuffer.AddAsrAudioData(int16ToFloat32(monoPCM))

					// 检查是否有足够的音频数据用于VAD检测
					if len(byteBuffer) >= frameBytes*vadNeedGetCount {
						// 处理所有完整帧
						frameCount := len(byteBuffer) / frameBytes
						haveVoice = false
						activeFrames := 0

						for i := 0; i < frameCount; i++ {
							start := i * frameBytes
							end := start + frameBytes
							frame := byteBuffer[start:end]

							active, err := state.VadProvider.IsVAD(frame)
							if err != nil {
								log.Errorf("VAD检测失败: %v", err)
								continue
							}

							if active {
								activeFrames++
							}
						}

						// 超过一半帧有语音才认为有语音活动
						haveVoice = activeFrames > frameCount/2

						// 移除已处理的帧
						if frameCount > 0 {
							byteBuffer = byteBuffer[frameCount*frameBytes:]
						}

						log.Debugf("VAD检测结果: haveVoice=%v, 活跃帧=%d/%d",
							haveVoice, activeFrames, frameCount)

						// 首次检测到语音时，获取所有缓存数据
						if haveVoice && !clientHaveVoice {
							allData := state.AsrAudioBuffer.GetAndClearAllData()
							state.AsrAudioChannel <- allData
						}
					}
				}

				if haveVoice {
					log.Infof("检测到语音, 样本数: %d", len(monoPCM))
					state.SetClientHaveVoice(true)
					state.SetClientHaveVoiceLastTime(time.Now().UnixMilli())

					// 发送到ASR处理
					if clientHaveVoice {
						floatData := int16ToFloat32(monoPCM)
						state.AsrAudioChannel <- floatData
					}
				} else {
					// 没有语音时清理缓存
					if !clientHaveVoice && state.AsrAudioBuffer.GetFrameCount() > vadNeedGetCount*3 {
						state.AsrAudioBuffer.RemoveAsrAudioData(1)
					}
				}

				// 静音检测逻辑
				lastHaveVoiceTime := state.GetClientHaveVoiceLastTime()
				if clientHaveVoice && lastHaveVoiceTime > 0 && !haveVoice {
					silenceDuration := time.Now().UnixMilli() - lastHaveVoiceTime
					if state.IsSilence(silenceDuration) {
						log.Info("检测到静音，停止ASR")
						state.SetClientVoiceStop(true)
						state.Asr.Stop()
						state.VadProvider.Reset()

						// 清空缓冲区
						state.AudioBuffer = nil
						byteBuffer = byteBuffer[:0]
					}
				}

			case <-state.Ctx.Done():
				return
			}
		}
	}()
}

// 初始化VAD
func initVAD(state *ClientState, format types_audio.AudioFormat) error {
	vadConfig := map[string]interface{}{
		"mode":              2,
		"sample_rate":       format.SampleRate,
		"frame_duration_ms": format.FrameDuration,
		"channels":          1, // VAD只需要单声道
	}

	vadInstance, err := vad.NewWebRTCVAD(vadConfig)
	if err != nil {
		return fmt.Errorf("创建VAD实例失败: %v", err)
	}

	state.VadProvider = vadInstance
	return nil
}

// 辅助函数：多声道转单声道
func convertToMono(data []int16, channels int) []int16 {
	if channels == 1 {
		return data
	}

	mono := make([]int16, len(data)/channels)
	for i := 0; i < len(mono); i++ {
		var sum int32
		for c := 0; c < channels; c++ {
			sum += int32(data[i*channels+c])
		}
		mono[i] = int16(sum / int32(channels))
	}
	return mono
}

// 辅助函数：int16 转字节 (小端序)
func int16ToBytes(data []int16) []byte {
	buf := make([]byte, len(data)*2)
	for i, v := range data {
		buf[i*2] = byte(v)        // 低字节
		buf[i*2+1] = byte(v >> 8) // 高字节
	}
	return buf
}

// 辅助函数：int16 转 float32 (用于ASR)
func int16ToFloat32(data []int16) []float32 {
	floatData := make([]float32, len(data))
	for i, v := range data {
		floatData[i] = float32(v) / 32768.0
	}
	return floatData
}

// handleTextMessage 处理文本消息
func HandleTextMessage(clientState *ClientState, message []byte) error {
	var clientMsg ClientMessage
	if err := json.Unmarshal(message, &clientMsg); err != nil {
		log.Errorf("解析消息失败: %v", err)
		return fmt.Errorf("解析消息失败: %v", err)
	}

	// 处理不同类型的消息
	switch clientMsg.Type {
	case MessageTypeHello:
		return handleHelloMessage(clientState, &clientMsg)
	case MessageTypeListen:
		return handleListenMessage(clientState, &clientMsg)
	case MessageTypeAbort:
		return handleAbortMessage(clientState, &clientMsg)
	case MessageTypeIot:
		return handleIoTMessage(clientState, &clientMsg)
	default:
		// 未知消息类型，直接回显
		return clientState.Conn.WriteMessage(websocket.TextMessage, message)
	}
}

// handleHelloMessage 处理 hello 消息
func handleHelloMessage(clientState *ClientState, msg *ClientMessage) error {
	// 创建新会话
	session, err := auth.A().CreateSession(msg.DeviceID)
	if err != nil {
		return fmt.Errorf("创建会话失败: %v", err)
	}

	// 更新客户端状态
	clientState.SessionID = session.ID

	clientState.InputAudioFormat = types_audio.AudioFormat{
		SampleRate:    msg.AudioParams.SampleRate,
		Channels:      msg.AudioParams.Channels,
		FrameDuration: msg.AudioParams.FrameDuration,
		Format:        msg.AudioParams.Format,
	}

	// 设置asr pcm帧大小, 输入音频格式, 给vad, asr使用
	clientState.SetAsrPcmFrameSize(clientState.InputAudioFormat.SampleRate, clientState.InputAudioFormat.Channels, clientState.InputAudioFormat.FrameDuration)

	ProcessVadAudio(clientState)

	// 发送 hello 响应
	response := ServerMessage{
		Type:        MessageTypeHello,
		Text:        "欢迎连接到小智服务器",
		SessionID:   session.ID,
		Transport:   "websocket",
		AudioFormat: &clientState.OutputAudioFormat,
	}

	return clientState.SendMsg(response)
}

func RecvAudio(clientState *ClientState, data []byte) bool {
	select {
	case clientState.OpusAudioBuffer <- data:
		return true
	default:
		log.Warnf("音频缓冲区已满, 丢弃音频数据")
	}
	return false
}

// handleListenMessage 处理监听消息
func handleListenMessage(clientState *ClientState, msg *ClientMessage) error {

	sessionID := clientState.SessionID

	// 根据状态处理
	switch msg.State {
	case MessageStateStart:
		handleListenStart(clientState, msg)
	case MessageStateStop:
		handleListenStop(clientState)
	case MessageStateDetect:
		// 唤醒词检测
		clientState.SetClientHaveVoice(false)

		// 如果有文本，处理唤醒词
		if msg.Text != "" {
			text := msg.Text
			// 移除标点符号和处理长度
			text = removePunctuation(text)

			// 检查是否是唤醒词
			isWakeupWord := isWakeupWord(text)
			enableGreeting := viper.GetBool("enable_greeting") // 从配置获取

			if isWakeupWord && !enableGreeting {
				// 如果是唤醒词，且关闭了唤醒词回复，发送 STT 消息后停止 TTS
				sttResponse := ServerMessage{
					Type:      ServerMessageTypeStt,
					Text:      text,
					SessionID: sessionID,
				}
				if err := clientState.SendMsg(sttResponse); err != nil {
					return fmt.Errorf("发送 STT 消息失败: %v", err)
				}
			} else {
				// 否则开始对话
				if err := startChat(clientState.GetSessionCtx(), clientState, text); err != nil {
					log.Errorf("开始对话失败: %v", err)
				}
			}
		}
	}

	// 记录日志
	log.Infof("设备 %s 更新音频监听状态: %s", msg.DeviceID, msg.State)
	return nil
}

// removePunctuation 移除文本中的标点符号
func removePunctuation(text string) string {
	// 创建一个字符串构建器
	var builder strings.Builder
	builder.Grow(len(text))

	for _, r := range text {
		if !unicode.IsPunct(r) && !unicode.IsSpace(r) {
			builder.WriteRune(r)
		}
	}

	return builder.String()
}

// isWakeupWord 检查文本是否是唤醒词
func isWakeupWord(text string) bool {
	wakeupWords := viper.GetStringSlice("wakeup_words")
	for _, word := range wakeupWords {
		if text == word {
			return true
		}
	}
	return false
}

// handleAbortMessage 处理中止消息
func handleAbortMessage(clientState *ClientState, msg *ClientMessage) error {
	sessionID := clientState.SessionID

	// 设置打断状态
	clientState.Abort = true
	clientState.Dialogue.Messages = nil // 清空对话历史
	clientState.CancelSessionCtx()

	Restart(clientState)

	// 发送中止响应
	response := ServerMessage{
		Type:      MessageTypeAbort,
		State:     MessageStateSuccess,
		SessionID: sessionID,
		Text:      "会话已中止",
	}

	// 发送响应
	if err := clientState.SendMsg(response); err != nil {
		return fmt.Errorf("发送响应失败: %v", err)
	}

	// 记录日志
	log.Infof("设备 %s 中止会话", msg.DeviceID)
	return nil
}

// handleIoTMessage 处理物联网消息
func handleIoTMessage(clientState *ClientState, msg *ClientMessage) error {
	// 获取客户端状态
	sessionID := clientState.SessionID

	// 验证设备ID
	/*
		if _, err := s.authManager.GetSession(msg.DeviceID); err != nil {
			return fmt.Errorf("会话验证失败: %v", err)
		}*/

	// 发送 IoT 响应
	response := ServerMessage{
		Type:      ServerMessageTypeIot,
		Text:      msg.Text,
		SessionID: sessionID,
		State:     MessageStateSuccess,
	}

	// 发送响应
	if err := clientState.SendMsg(response); err != nil {
		return fmt.Errorf("发送响应失败: %v", err)
	}

	// 记录日志
	log.Infof("设备 %s 物联网指令: %s", msg.DeviceID, msg.Text)
	return nil
}

// startChat 开始对话
func startChat(ctx context.Context, clientState *ClientState, text string) error {
	// 获取客户端状态

	sessionID := clientState.SessionID

	requestMessages, err := llm_memory.Get().GetMessagesForLLM(ctx, clientState.DeviceID, 10)
	if err != nil {
		log.Errorf("获取对话历史失败: %v", err)
	}

	requestMessages = append(requestMessages, llm_common.Message{
		Role:    "user",
		Content: text,
	})

	// 添加用户消息到对话历史
	llm_memory.Get().AddMessage(ctx, clientState.DeviceID, "user", text)

	ctx, cancel := context.WithCancel(ctx)
	_ = cancel

	// 发送 LLM 请求
	responseSentences, err := llm.HandleLLMWithContext(
		ctx,
		clientState.LLMProvider,
		messagesToInterfaces(requestMessages),
		sessionID,
	)
	if err != nil {
		log.Errorf("发送 LLM 请求失败, seesionID: %s, error: %v", sessionID, err)
		return fmt.Errorf("发送 LLM 请求失败: %v", err)
	}

	go func() {
		ok, err := HandleLLMResponse(ctx, clientState, responseSentences)
		if err != nil {
			cancel()
		}

		_ = ok
		/*
			if ok {
				s.handleContinueChat(state)
			}*/
	}()

	return nil
}

// 添加一个转换函数
func messagesToInterfaces(msgs []llm_common.Message) []interface{} {
	result := make([]interface{}, len(msgs))
	for i, msg := range msgs {
		result[i] = msg
	}
	return result
}
