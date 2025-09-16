package chat

import (
	"context"
	"fmt"
	"time"
	. "xiaozhi-esp32-server-golang/internal/data/client"
	i_redis "xiaozhi-esp32-server-golang/internal/db/redis"
	"xiaozhi-esp32-server-golang/internal/domain/audio"
	llm_common "xiaozhi-esp32-server-golang/internal/domain/llm/common"
	"xiaozhi-esp32-server-golang/internal/util"
	log "xiaozhi-esp32-server-golang/logger"
)

type TTSQueueItem struct {
	ctx         context.Context
	llmResponse llm_common.LLMResponseStruct
	onStartFunc func()
	onEndFunc   func(err error)
}

// TTSManager 负责TTS相关的处理
// 可以根据需要扩展字段
// 目前无状态，但可后续扩展

type TTSManagerOption func(*TTSManager)

type TTSManager struct {
	clientState     *ClientState
	serverTransport *ServerTransport
	ttsQueue        *util.Queue[TTSQueueItem]
}

// NewTTSManager 只接受WithClientState
func NewTTSManager(clientState *ClientState, serverTransport *ServerTransport, opts ...TTSManagerOption) *TTSManager {
	t := &TTSManager{
		clientState:     clientState,
		serverTransport: serverTransport,
		ttsQueue:        util.NewQueue[TTSQueueItem](10),
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// 启动TTS队列消费协程
func (t *TTSManager) Start(ctx context.Context) {
	t.processTTSQueue(ctx)
}

func (t *TTSManager) processTTSQueue(ctx context.Context) {
	for {
		item, err := t.ttsQueue.Pop(ctx, 0) // 阻塞式
		if err != nil {
			if err == util.ErrQueueCtxDone {
				return
			}
			continue
		}
		if item.onStartFunc != nil {
			item.onStartFunc()
		}
		err = t.handleTts(item.ctx, item.llmResponse)
		if item.onEndFunc != nil {
			item.onEndFunc(err)
		}
	}
}

func (t *TTSManager) ClearTTSQueue() {
	t.ttsQueue.Clear()
}

// 处理文本内容响应（异步 TTS 入队）
func (t *TTSManager) handleTextResponse(ctx context.Context, llmResponse llm_common.LLMResponseStruct, isSync bool) error {
	if llmResponse.Text == "" {
		return nil
	}

	ttsQueueItem := TTSQueueItem{ctx: ctx, llmResponse: llmResponse}
	endChan := make(chan bool, 1)
	ttsQueueItem.onEndFunc = func(err error) {
		select {
		case endChan <- true:
		default:
		}
	}

	t.ttsQueue.Push(ttsQueueItem)

	if isSync {
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case <-endChan:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("TTS 处理上下文已取消")
		case <-timer.C:
			return fmt.Errorf("TTS 处理超时")
		}
	}

	return nil
}

// 同步 TTS 处理
func (t *TTSManager) handleTts(ctx context.Context, llmResponse llm_common.LLMResponseStruct) error {
	log.Debugf("handleTts start, text: %s", llmResponse.Text)
	if llmResponse.Text == "" {
		return nil
	}

	// 使用带上下文的TTS处理
	outputChan, err := t.clientState.TTSProvider.TextToSpeechStream(ctx, llmResponse.Text, t.clientState.OutputAudioFormat.SampleRate, t.clientState.OutputAudioFormat.Channels, t.clientState.OutputAudioFormat.FrameDuration)
	if err != nil {
		log.Errorf("生成 TTS 音频失败: %v", err)
		return fmt.Errorf("生成 TTS 音频失败: %v", err)
	}

	if err := t.serverTransport.SendSentenceStart(llmResponse.Text); err != nil {
		log.Errorf("发送 TTS 文本失败: %s, %v", llmResponse.Text, err)
		return fmt.Errorf("发送 TTS 文本失败: %s, %v", llmResponse.Text, err)
	}

	// 发送音频帧
	if err := t.SendTTSAudio(ctx, outputChan, llmResponse.IsStart); err != nil {
		log.Errorf("发送 TTS 音频失败: %s, %v", llmResponse.Text, err)
		return fmt.Errorf("发送 TTS 音频失败: %s, %v", llmResponse.Text, err)
	}

	if err := t.serverTransport.SendSentenceEnd(llmResponse.Text); err != nil {
		log.Errorf("发送 TTS 文本失败: %s, %v", llmResponse.Text, err)
		return fmt.Errorf("发送 TTS 文本失败: %s, %v", llmResponse.Text, err)
	}

	return nil
}

// getAlignedDuration 计算当前时间与开始时间的差值，向上对齐到frameDuration
func getAlignedDuration(startTime time.Time, frameDuration time.Duration) time.Duration {
	elapsed := time.Since(startTime)
	// 向上对齐到frameDuration
	alignedMs := ((elapsed.Milliseconds() + frameDuration.Milliseconds() - 1) / frameDuration.Milliseconds()) * frameDuration.Milliseconds()
	return time.Duration(alignedMs) * time.Millisecond
}

func (t *TTSManager) SendTTSAudio(ctx context.Context, audioChan chan []byte, isStart bool) error {
	totalFrames := 0 // 跟踪已发送的总帧数

	isStatistic := true
	//首次发送180ms音频, 根据outputAudioFormat.FrameDuration计算
	cacheFrameCount := 120 / t.clientState.OutputAudioFormat.FrameDuration
	/*if cacheFrameCount > 20 || cacheFrameCount < 3 {
		cacheFrameCount = 5
	}*/

	// 创建用于流控的缓冲通道
	flowControlChan := make(chan []byte, 1000) // 大缓冲区避免阻塞

	// 启动数字人音频处理协程
	metaAudioChan := make(chan []byte, 1000)
	go t.SendAudioToMetaHuman(ctx, metaAudioChan)
	defer func() {
		close(metaAudioChan)
	}()

	// 启动流控处理协程
	go t.processFlowControl(ctx, flowControlChan, cacheFrameCount, isStart, &isStatistic, &totalFrames)
	defer func() {
		close(flowControlChan)
	}()

	log.Debugf("SendTTSAudio 开始，缓存帧数: %d", cacheFrameCount)

	// 主循环：立即分发数据，不进行流控等待
	for {
		select {
		case <-ctx.Done():
			log.Debugf("SendTTSAudio context done, exit")
			return nil
		case frame, ok := <-audioChan:
			if !ok {
				log.Debugf("SendTTSAudio audioChan closed, exit")
				return nil
			}

			// 1. 立即分发给metaAudioChan（非阻塞）
			select {
			case metaAudioChan <- frame:
				// 成功发送到数字人队列
			default:
				// metaAudioChan已满，跳过此帧（不阻塞主流程）
				log.Debugf("metaAudioChan 已满，跳过帧")
			}

			// 2. 分发给流控逻辑（非阻塞）
			select {
			case flowControlChan <- frame:
				// 成功发送到流控队列
			default:
				// 流控队列已满，记录警告但不阻塞
				log.Warnf("flowControlChan 已满，可能影响流控")
			}
		}
	}
}

// 独立的流控处理协程
func (t *TTSManager) processFlowControl(ctx context.Context, flowControlChan chan []byte, cacheFrameCount int, isStart bool, isStatistic *bool, totalFrames *int) {
	// 记录开始发送的时间戳
	startTime := time.Now()

	// 基于绝对时间的精确流控
	frameDuration := time.Duration(t.clientState.OutputAudioFormat.FrameDuration) * time.Millisecond

	log.Debugf("processFlowControl 开始，缓存帧数: %d, 帧时长: %v", cacheFrameCount, frameDuration)

	// 使用滑动窗口机制，确保对端始终缓存 cacheFrameCount 帧数据
	for {
		// 计算下一帧应该发送的时间点
		nextFrameTime := startTime.Add(time.Duration(*totalFrames-cacheFrameCount) * frameDuration)
		now := time.Now()

		// 如果下一帧时间还没到，需要等待
		if now.Before(nextFrameTime) {
			sleepDuration := nextFrameTime.Sub(now)
			//log.Debugf("processFlowControl 流控等待: %v", sleepDuration)
			time.Sleep(sleepDuration)
		}

		// 尝试获取并发送下一帧
		select {
		case <-ctx.Done():
			log.Debugf("processFlowControl context done, exit")
			return
		case frame, ok := <-flowControlChan:
			if !ok {
				// 通道已关闭，所有帧已处理完毕
				// 为确保终端播放完成：等待已发送帧的总时长与从开始发送以来的实际耗时之间的差值
				elapsed := time.Since(startTime)
				totalDuration := time.Duration(*totalFrames) * frameDuration
				if totalDuration > elapsed {
					waitDuration := totalDuration - elapsed
					log.Debugf("processFlowControl 等待客户端播放剩余缓冲: %v (totalFrames=%d, frameDuration=%v)", waitDuration, *totalFrames, frameDuration)
					time.Sleep(waitDuration)
				}
				log.Debugf("processFlowControl flowControlChan closed, exit, 总共发送 %d 帧", *totalFrames)
				return
			}

			// 发送当前帧到客户端
			if err := t.serverTransport.SendAudio(frame); err != nil {
				log.Errorf("发送 TTS 音频失败: 第 %d 帧, len: %d, 错误: %v", *totalFrames, len(frame), err)
				return
			}

			*totalFrames++
			if *totalFrames%100 == 0 {
				log.Debugf("processFlowControl 已发送 %d 帧", *totalFrames)
			}

			// 统计信息记录（仅在开始时记录一次）
			if isStart && *isStatistic && *totalFrames == 1 {
				log.Debugf("从接收音频结束 asr->llm->tts首帧 整体 耗时: %d ms", t.clientState.GetAsrLlmTtsDuration())
				*isStatistic = false
			}
		}
	}
}

// 发送音频到数字人 redis队列
func (t *TTSManager) SendAudioToMetaHuman(ctx context.Context, audioChan chan []byte) error {
	redisClient := i_redis.GetClient()

	if redisClient == nil {
		log.Errorf("获取Redis客户端失败")
		return fmt.Errorf("获取Redis客户端失败")
	}

	audioProcesser, err := audio.GetAudioProcesser(16000, 1, 60)
	if err != nil {
		return fmt.Errorf("创建音频处理器失败: %v", err)
	}

	pcmFrame := make([]float32, 16000*1*60/1000)

	queueKey := "DHQA_AUDIO_QUEUE"

	// 音频缓冲区：积累1000ms的数据
	// 16000采样率 * 1声道 * 1000ms = 16000个样本 = 32000字节
	bufferSize := 16000 * 2 // 1000ms的PCM数据（每个样本2字节）
	audioBuffer := make([]byte, 0, bufferSize)

	// 写入缓冲区的函数
	writeBuffer := func() {
		if len(audioBuffer) > 0 {
			redisClient.RPush(ctx, queueKey, audioBuffer)
			log.Debugf("写入Redis音频数据: %d 字节", len(audioBuffer))
			audioBuffer = audioBuffer[:0] // 清空缓冲区
		}
	}

	for {
		select {
		case <-ctx.Done():
			// 上下文取消时，写入剩余数据
			writeBuffer()
			return nil
		case opusFrame, ok := <-audioChan:
			if !ok {
				// 通道关闭时，写入剩余数据
				writeBuffer()
				return nil
			}

			n, err := audioProcesser.DecoderFloat32(opusFrame, pcmFrame)
			if err != nil {
				log.Errorf("解码失败: %v", err)
				return fmt.Errorf("解码失败: %v", err)
			}

			// 为PCM字节数据分配足够的空间（每个float32样本需要2个字节）
			pcmBytes := make([]byte, n*2)
			util.Float32ToPCMBytes(pcmFrame[:n], pcmBytes)

			// 将PCM数据添加到缓冲区
			audioBuffer = append(audioBuffer, pcmBytes...)

			// 当缓冲区积累到1000ms数据时，写入Redis
			if len(audioBuffer) >= bufferSize {
				writeBuffer()
			}
		}
	}
}
