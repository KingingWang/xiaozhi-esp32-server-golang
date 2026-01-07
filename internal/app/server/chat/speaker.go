package chat

import (
	"context"

	"xiaozhi-esp32-server-golang/internal/domain/speaker"
)

// SpeakerManager 声纹识别管理器（包装 SpeakerProvider）
type SpeakerManager struct {
	provider speaker.SpeakerProvider
}

// NewSpeakerManager 创建声纹管理器
func NewSpeakerManager(provider speaker.SpeakerProvider) *SpeakerManager {
	return &SpeakerManager{
		provider: provider,
	}
}

// StartStreaming 启动流式识别
func (sm *SpeakerManager) StartStreaming(ctx context.Context, sampleRate int, agentId string) error {
	return sm.provider.StartStreaming(ctx, sampleRate, agentId)
}

// SendAudioChunk 发送音频块
func (sm *SpeakerManager) SendAudioChunk(ctx context.Context, pcmData []float32) error {
	return sm.provider.SendAudioChunk(ctx, pcmData)
}

// FinishAndIdentify 完成识别并获取结果
func (sm *SpeakerManager) FinishAndIdentify(ctx context.Context) (*speaker.IdentifyResult, error) {
	return sm.provider.FinishAndIdentify(ctx)
}

// Close 关闭声纹管理器
func (sm *SpeakerManager) Close() error {
	return sm.provider.Close()
}

// IsActive 检查是否处于激活状态
func (sm *SpeakerManager) IsActive() bool {
	return sm.provider.IsActive()
}
