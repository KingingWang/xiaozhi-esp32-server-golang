package server

import (
	"fmt"
	. "xiaozhi-esp32-server-golang/internal/data/client"
	"xiaozhi-esp32-server-golang/internal/domain/eventbus"
	"xiaozhi-esp32-server-golang/internal/domain/memory/llm_memory"
	workpool "xiaozhi-esp32-server-golang/internal/util/work"
	log "xiaozhi-esp32-server-golang/logger"

	"github.com/cloudwego/eino/schema"
)

type EventHandle struct {
}

func NewEventHandle() *EventHandle {
	return &EventHandle{}
}

func (s *EventHandle) Start() error {
	go s.HandleAddMessage()
	go s.HandleSessionEnd()
	return nil
}

func (s *EventHandle) HandleAddMessage() {
	type AddMessageJob struct {
		clientState *ClientState
		Msg         schema.Message
	}
	f := func(job workpool.Job) error {
		addMessageJob, ok := job.(AddMessageJob)
		if !ok {
			return fmt.Errorf("invalid job info")
		}
		clientState := addMessageJob.clientState
		//添加到 消息列表中
		llm_memory.Get().AddMessage(clientState.Ctx, clientState.DeviceID, clientState.AgentID, addMessageJob.Msg)
		//将消息加到 长期记忆体中
		if clientState.MemoryProvider != nil {
			err := clientState.MemoryProvider.AddMessage(clientState.Ctx, clientState.GetDeviceIDOrAgentID(), addMessageJob.Msg)
			if err != nil {
				return fmt.Errorf("add message to memory provider failed: %w", err)
			}
		}
		return nil
	}
	workPool := workpool.NewWorkPool(10, 1000, workpool.JobHandler(f))
	eventbus.Get().Subscribe(eventbus.TopicAddMessage, func(clientState *ClientState, msg schema.Message) {
		workPool.Submit(AddMessageJob{
			clientState: clientState,
			Msg:         msg,
		})
	})
}

func (s *EventHandle) HandleSessionEnd() error {
	f := func(job workpool.Job) error {
		clientState, ok := job.(*ClientState)
		if !ok {
			return fmt.Errorf("invalid job info")
		}

		//将消息加到 长期记忆体中
		if clientState.MemoryProvider != nil {
			err := clientState.MemoryProvider.Flush(clientState.Ctx, clientState.GetDeviceIDOrAgentID())
			if err != nil {
				return fmt.Errorf("add message to memory provider failed: %w", err)
			}
		}
		return nil
	}
	workPool := workpool.NewWorkPool(10, 1000, workpool.JobHandler(f))
	eventbus.Get().Subscribe(eventbus.TopicSessionEnd, func(clientState *ClientState) {
		if clientState == nil {
			log.Warnf("HandleSessionEnd: clientState is nil, skipping")
			return
		}
		if clientState.MemoryProvider == nil {
			return
		}
		log.Infof("HandleSessionEnd: deviceId: %s", clientState.DeviceID)
		workPool.Submit(clientState)
	})
	return nil
}
