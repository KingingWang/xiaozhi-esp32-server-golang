package chat

import (
	"context"
	"fmt"
	"time"
	. "xiaozhi-esp32-server-golang/internal/data/client"
	"xiaozhi-esp32-server-golang/internal/domain/audio"
	log "xiaozhi-esp32-server-golang/logger"

	"github.com/spf13/viper"
)

type ASRManagerOption func(*ASRManager)

type ASRManager struct {
	clientState     *ClientState
	serverTransport *ServerTransport
	session         *ChatSession // 用于访问 speakerManager
}

func NewASRManager(clientState *ClientState, serverTransport *ServerTransport, opts ...ASRManagerOption) *ASRManager {
	asr := &ASRManager{
		clientState:     clientState,
		serverTransport: serverTransport,
		session:         nil, // 稍后通过 SetSession 设置
	}
	for _, opt := range opts {
		opt(asr)
	}
	return asr
}

// ProcessVadAudio 启动VAD音频处理
func (a *ASRManager) ProcessVadAudio(ctx context.Context, onClose func()) {
	state := a.clientState
	go func() {
		hasTriggeredCancel := true // 标志位，记录是否已触发过取消操作（当 voiceDuration > 120 时）
		audioFormat := state.InputAudioFormat
		audioProcesser, err := audio.GetAudioProcesser(audioFormat.SampleRate, audioFormat.Channels, audioFormat.FrameDuration)
		if err != nil {
			log.Errorf("获取解码器失败: %v", err)
			return
		}
		frameSize := state.AsrAudioBuffer.PcmFrameSize

		vadNeedGetCount := 1
		if state.DeviceConfig.Vad.Provider == "silero_vad" {
			vadNeedGetCount = 60 / audioFormat.FrameDuration
		}

		for {
			pcmFrame := make([]float32, frameSize)

			select {
			case opusFrame, ok := <-state.OpusAudioBuffer:
				//log.Debugf("processAsrAudio 收到音频数据, len: %d", len(opusFrame))
				if !ok {
					log.Debugf("processAsrAudio 音频通道已关闭")
					return
				}

				var skipVad bool
				var haveVoice bool
				clientHaveVoice := state.GetClientHaveVoice()
				if state.Asr.AutoEnd || state.ListenMode == "manual" {
					skipVad = true         //跳过vad
					clientHaveVoice = true //之前有声音
					haveVoice = true       //本次有声音
				}

				if state.GetClientVoiceStop() { //已停止 说话 则不接收音频数据
					//log.Infof("客户端停止说话, 跳过音频数据")
					continue
				}

				//log.Debugf("clientVoiceStop: %+v, asrDataSize: %d, listenMode: %s, isSkipVad: %v\n", state.GetClientVoiceStop(), state.AsrAudioBuffer.GetAsrDataSize(), state.ListenMode, skipVad)

				n, err := audioProcesser.DecoderFloat32(opusFrame, pcmFrame)
				if err != nil {
					log.Errorf("解码失败: %v", err)
					continue
				}

				var vadPcmData []float32
				pcmData := pcmFrame[:n]
				if !skipVad {
					//decode opus to pcm
					state.AsrAudioBuffer.AddAsrAudioData(pcmData)

					if state.AsrAudioBuffer.GetAsrDataSize() >= vadNeedGetCount*state.AsrAudioBuffer.PcmFrameSize {
						//如果要进行vad, 至少要取60ms的音频数据
						vadPcmData = state.AsrAudioBuffer.GetAsrData(vadNeedGetCount)

						//如果已经检测到语音, 则不进行vad检测, 直接将pcmData传给asr
						if state.Vad.VadProvider == nil {
							// 初始化vad
							err = state.Vad.Init(state.DeviceConfig.Vad.Provider, state.DeviceConfig.Vad.Config)
							if err != nil {
								log.Errorf("初始化vad失败: %v", err)
								continue
							}
						}
						err = state.Vad.ResetVad()
						if err != nil {
							log.Errorf("重置vad失败: %v", err)
							continue
						}
						haveVoice, err = state.Vad.IsVADExt(vadPcmData, audioFormat.SampleRate, frameSize)
						if err != nil {
							log.Errorf("processAsrAudio VAD检测失败: %v", err)
							//删除
							continue
						}
						//首次触发识别到语音时,为了语音数据完整性 将vadPcmData赋值给pcmData, 之后的音频数据全部进入asr
						if haveVoice && !clientHaveVoice {
							//首次检测到语音时，最多只保留200ms的前静音数据
							allData := state.AsrAudioBuffer.GetAndClearAllData()
							pcmData = allData
						}
					}
					//log.Debugf("isVad, pcmData len: %d, vadPcmData len: %d, haveVoice: %v", len(pcmData), len(vadPcmData), haveVoice)
				}

				if !haveVoice || state.Asr.AutoEnd {
					state.Vad.AddIdleDuration(int64(audioFormat.FrameDuration))
					idleDuration := state.Vad.GetIdleDuration()
					log.Infof("空闲时间: %dms", idleDuration)
					if idleDuration > state.GetMaxIdleDuration() {
						log.Infof("超出空闲时长: %dms, 断开连接", idleDuration)
						//断开连接
						onClose()
						return
					}
				}

				if haveVoice {
					//log.Infof("检测到语音, len: %d", len(pcmData))
					state.SetClientHaveVoice(true)
					state.SetClientHaveVoiceLastTime(time.Now().UnixMilli())
					if !state.Asr.AutoEnd {
						state.Vad.ResetIdleDuration()
					}
					// 累积检测到声音的时长（同时更新一次过程中的时长）
					state.Vad.AddVoiceDuration(int64(audioFormat.FrameDuration))

					voiceDuration := state.Vad.GetVoiceDuration()
					if state.IsRealTime() && viper.GetInt("chat.realtime_mode") == RealTimeModeVadInterrupt && voiceDuration > 360 {
						// 只有在未触发过的情况下才执行，确保只执行一次
						if !hasTriggeredCancel {
							//realtime模式下, 如果此时有正在进行的llm和tts则取消掉
							log.Debugf("realtime模式vad打断下 && 语音时长超过%d ms 如果此时有正在进行的llm和tts则取消掉", voiceDuration)
							state.AfterAsrSessionCtx.Cancel()
							hasTriggeredCancel = true // 标记为已触发
						}
					}
				} else {
					// 没有声音时，如果之前也没有语音，则重置累积的声音时长
					// 如果之前有语音但本次没有，保留时长值，让后续逻辑判断是否应该重置
					if !clientHaveVoice {
						state.Vad.ResetVoiceDuration()
						//保留近10帧
						/*
							if state.AsrAudioBuffer.GetFrameCount() > vadNeedGetCount*3 {
								state.AsrAudioBuffer.RemoveAsrAudioData(1)
							}*/
						continue
					}
				}

				if clientHaveVoice {
					//vad识别成功, 往asr音频通道里发送数据
					//log.Infof("vad识别成功, 往asr音频通道里发送数据, len: %d", len(pcmData))
					state.Asr.AddAudioData(pcmData)

					// 如果启用声纹识别，同时发送到声纹识别服务
					// 需要同时满足：全局开关启用、设备配置中有声纹组、speakerManager已初始化
					if state.IsSpeakerEnabled() && state.HasSpeakerGroups() &&
						a.session != nil && a.session.speakerManager != nil {
						// 首次检测到语音时，启动流式识别
						if !a.session.speakerManager.IsActive() {
							sampleRate := audioFormat.SampleRate
							agentId := a.session.clientState.AgentID
							if err := a.session.speakerManager.StartStreaming(ctx, sampleRate, agentId); err != nil {
								log.Warnf("启动声纹识别流失败: %v", err)
							}
						}

						// 发送音频块
						if err := a.session.speakerManager.SendAudioChunk(ctx, pcmData); err != nil {
							log.Warnf("发送音频块到声纹识别服务失败: %v", err)
						}
					}
				}

				//已经有语音了, 但本次没有检测到语音, 则需要判断是否已经停止说话
				lastHaveVoiceTime := state.GetClientHaveVoiceLastTime()

				if clientHaveVoice && lastHaveVoiceTime > 0 && !haveVoice {
					// 判断有音频的语音时长，如果小于300ms则重置clientHaveVoice，避免短时间语音造成的误判
					voiceDurationInSession := state.Vad.GetVoiceDurationInSession()
					if voiceDurationInSession < 300 {
						log.Debugf("语音时长过短 (%dms < 300ms)，重置clientHaveVoice", voiceDurationInSession)
						state.SetClientHaveVoice(false)
						state.Vad.ResetVoiceDuration()
						continue
					}

					idleDuration := state.Vad.GetIdleDuration()
					if state.IsSilence(idleDuration) { //从有声音到 静默的判断
						// 在 OnVoiceSilence 之前重置标志位，以便下次可以再次触发
						hasTriggeredCancel = false
						state.OnVoiceSilence()
						state.VoiceStatus.Reset()
						continue
					}
				}

			case <-ctx.Done():
				return
			}
		}
	}()
}

// restartAsrRecognition 重启ASR识别
func (a *ASRManager) RestartAsrRecognition(ctx context.Context) error {
	state := a.clientState
	log.Debugf("重启ASR识别开始")

	// 取消当前ASR上下文
	if state.Asr.Cancel != nil {
		state.Asr.Cancel()
	}

	state.VoiceStatus.Reset()
	state.AsrAudioBuffer.ClearAsrAudioData()
	state.Asr.ClearHistoryAudio() // 清空历史音频缓存

	// 等待一小段时间让资源清理
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// 重新创建ASR上下文和通道
	state.Asr.Ctx, state.Asr.Cancel = context.WithCancel(ctx)
	state.Asr.AsrAudioChannel = make(chan []float32, 100)

	// 重新启动流式识别
	asrResultChannel, err := state.AsrProvider.StreamingRecognize(state.Asr.Ctx, state.Asr.AsrAudioChannel)
	if err != nil {
		log.Errorf("重启ASR流式识别失败: %v", err)
		return fmt.Errorf("重启ASR流式识别失败: %v", err)
	}

	state.AsrResultChannel = asrResultChannel
	log.Debugf("重启ASR识别成功")
	return nil
}
