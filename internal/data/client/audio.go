package client

import (
	"context"
	"sync"
	"time"
	log "xiaozhi-esp32-server-golang/logger"
)

const (
	AudioChannelStateIdle    = "idle"
	AudioChannelStatePlaying = "playing"
	AudioChannelStatePause   = "pause"
)

type AudioState struct {
	Lock  sync.Mutex
	State string     // playing, pause, stop
	Cond  *sync.Cond // 修改为指针类型，需要手动初始化

	Ctx    context.Context
	Cancel context.CancelFunc
}

// NewAudioState 创建并正确初始化 AudioState
func NewAudioState() *AudioState {
	audioState := &AudioState{
		State: AudioChannelStateIdle,
	}
	audioState.Cond = sync.NewCond(&audioState.Lock)
	return audioState
}

func (a *AudioState) SetPause() error {
	startTime := time.Now()
	a.Lock.Lock()
	lockTime := time.Since(startTime)

	defer a.Lock.Unlock()

	if a.State == AudioChannelStatePlaying {
		oldState := a.State
		a.State = AudioChannelStatePause
		a.Cond.Broadcast() // 唤醒等待的 goroutine
		log.Infof("[AudioState] SetPause: %s->%s, 锁耗时: %v", oldState, a.State, lockTime)
	}
	return nil
}

func (a *AudioState) GetState() string {
	return a.State
}

func (a *AudioState) SetStop() error {
	startTime := time.Now()
	a.Lock.Lock()
	lockTime := time.Since(startTime)

	defer a.Lock.Unlock()

	if a.State == AudioChannelStatePlaying || a.State == AudioChannelStatePause {
		oldState := a.State
		a.State = AudioChannelStateIdle
		a.Cond.Broadcast() // 唤醒等待的 goroutine
		log.Infof("[AudioState] SetStop: %s->%s, 锁耗时: %v", oldState, a.State, lockTime)
	}

	return nil
}

func (a *AudioState) SetPlay() error {
	startTime := time.Now()
	a.Lock.Lock()
	lockTime := time.Since(startTime)

	defer a.Lock.Unlock()

	oldState := a.State
	a.State = AudioChannelStatePlaying
	a.Cond.Broadcast() // 总是唤醒等待的 goroutine
	log.Infof("[AudioState] SetPlay: %s->%s, 锁耗时: %v", oldState, a.State, lockTime)

	return nil
}

func (a *AudioState) CancelCtx() error {
	a.Lock.Lock()
	defer a.Lock.Unlock()

	a.Cancel()
	a.State = AudioChannelStateIdle
	return nil
}

func (a *AudioState) RestartSessionCtx() error {
	a.Lock.Lock()
	defer a.Lock.Unlock()

	a.Cancel()
	a.State = AudioChannelStateIdle

	a.Ctx, a.Cancel = context.WithCancel(context.Background())

	return nil
}
