package edge_offline

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"xiaozhi-esp32-server-golang/internal/domain/tts/common"
	log "xiaozhi-esp32-server-golang/logger"

	"github.com/gopxl/beep"
	"github.com/gorilla/websocket"
	pool "github.com/jolestar/go-commons-pool/v2"
)

var (
	// 全局连接池实例
	globalPool *pool.ObjectPool
	// 全局连接池配置
	globalPoolConfig = &pool.ObjectPoolConfig{
		MaxTotal:                10,
		MaxIdle:                 5,
		MinIdle:                 1,
		TestOnBorrow:            true,
		TestOnReturn:            true,
		TestWhileIdle:           true,
		MinEvictableIdleTime:    time.Minute,
		TimeBetweenEvictionRuns: time.Minute,
	}
	// 确保全局连接池只初始化一次
	initOnce sync.Once
)

// printPoolStats 打印连接池状态
func printPoolStats() {
	if globalPool == nil {
		return
	}
	log.Infof("连接池状态 - 活跃连接数: %d, 空闲连接数: %d",
		globalPool.GetNumActive(), globalPool.GetNumIdle())
}

// InitGlobalPool 初始化全局连接池
func InitGlobalPool(serverURL string) {
	initOnce.Do(func() {
		factory := &wsConnFactory{
			serverURL: serverURL,
			dialer: &websocket.Dialer{
				HandshakeTimeout: 10 * time.Second,
			},
		}

		globalPool = pool.NewObjectPool(context.Background(), factory, globalPoolConfig)

		// 启动定期打印连接池状态的goroutine
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()

			for range ticker.C {
				printPoolStats()
			}
		}()
	})
}

// WebSocket连接工厂
type wsConnFactory struct {
	serverURL string
	dialer    *websocket.Dialer
}

func (f *wsConnFactory) MakeObject(ctx context.Context) (*pool.PooledObject, error) {
	conn, _, err := f.dialer.Dial(f.serverURL, nil)
	if err != nil {
		return nil, fmt.Errorf("WebSocket连接失败: %v", err)
	}
	return pool.NewPooledObject(&wsConnWrapper{
		conn:         conn,
		lastActiveAt: time.Now(),
	}), nil
}

func (f *wsConnFactory) DestroyObject(ctx context.Context, object *pool.PooledObject) error {
	wrapper := object.Object.(*wsConnWrapper)
	return wrapper.conn.Close()
}

func (f *wsConnFactory) ValidateObject(ctx context.Context, object *pool.PooledObject) bool {
	wrapper := object.Object.(*wsConnWrapper)
	return time.Since(wrapper.lastActiveAt) < 30*time.Second
}

func (f *wsConnFactory) ActivateObject(ctx context.Context, object *pool.PooledObject) error {
	wrapper := object.Object.(*wsConnWrapper)
	wrapper.lastActiveAt = time.Now()
	return nil
}

func (f *wsConnFactory) PassivateObject(ctx context.Context, object *pool.PooledObject) error {
	return nil
}

const (
	// WebSocket缓冲区大小
	wsBufferSize = 1024 * 1024 // 1MB
)

// wsConnWrapper WebSocket 连接包装器
type wsConnWrapper struct {
	conn         *websocket.Conn
	lastActiveAt time.Time
}

// EdgeOfflineTTSProvider WebSocket TTS 提供者
type EdgeOfflineTTSProvider struct {
	ServerURL string
	Timeout   time.Duration
}

// NewEdgeOfflineTTSProvider 创建新的 Edge Offline TTS 提供者
func NewEdgeOfflineTTSProvider(config map[string]interface{}) *EdgeOfflineTTSProvider {
	serverURL, _ := config["server_url"].(string)
	timeout, _ := config["timeout"].(float64)

	// 设置默认值
	if serverURL == "" {
		serverURL = "ws://localhost:8080/tts"
	}
	if timeout == 0 {
		timeout = 30 // 默认30秒超时
	}

	// 初始化全局连接池
	InitGlobalPool(serverURL)

	return &EdgeOfflineTTSProvider{
		ServerURL: serverURL,
		Timeout:   time.Duration(timeout) * time.Second,
	}
}

// getConnection 从连接池获取连接
func (p *EdgeOfflineTTSProvider) getConnection(ctx context.Context) (*wsConnWrapper, error) {
	if globalPool == nil {
		return nil, fmt.Errorf("全局连接池未初始化")
	}

	obj, err := globalPool.BorrowObject(ctx)
	if err != nil {
		return nil, fmt.Errorf("从连接池获取连接失败: %v", err)
	}
	return obj.(*wsConnWrapper), nil
}

// returnConnection 归还连接到连接池
func (p *EdgeOfflineTTSProvider) returnConnection(wrapper *wsConnWrapper) error {
	if globalPool == nil {
		return fmt.Errorf("全局连接池未初始化")
	}
	ctx := context.Background()
	return globalPool.ReturnObject(ctx, wrapper)
}

// removeConnection 从连接池中移除连接
func (p *EdgeOfflineTTSProvider) removeConnection(wrapper *wsConnWrapper) {
	if wrapper == nil || globalPool == nil {
		return
	}
	ctx := context.Background()
	globalPool.InvalidateObject(ctx, wrapper)
}

// TextToSpeech 将文本转换为语音，返回音频帧数据
func (p *EdgeOfflineTTSProvider) TextToSpeech(ctx context.Context, text string, sampleRate int, channels int, frameDuration int) ([][]byte, error) {
	var frames [][]byte

	// 获取连接
	wrapper, err := p.getConnection(ctx)
	if err != nil {
		return nil, err
	}

	// 发送文本
	err = wrapper.conn.WriteMessage(websocket.TextMessage, []byte(text))
	if err != nil {
		p.removeConnection(wrapper)
		return nil, fmt.Errorf("发送文本失败: %v", err)
	}

	// 创建管道用于音频数据传输
	pipeReader, pipeWriter := io.Pipe()
	outputChan := make(chan []byte, 1000)
	startTs := time.Now().UnixMilli()

	// 创建音频解码器
	audioDecoder, err := common.CreateAudioDecoder(ctx, pipeReader, outputChan, frameDuration, "mp3")
	if err != nil {
		pipeReader.Close()
		return nil, fmt.Errorf("创建音频解码器失败: %v", err)
	}

	// 启动解码器
	go func() {
		if err := audioDecoder.Run(startTs); err != nil {
			log.Errorf("音频解码失败: %v", err)
		}
	}()

	// 接收WebSocket数据并写入管道
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer pipeWriter.Close()

		for {
			messageType, data, err := wrapper.conn.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					return
				}
				log.Errorf("读取WebSocket消息失败: %v", err)
				return
			}

			if messageType == websocket.BinaryMessage {
				if _, err := pipeWriter.Write(data); err != nil {
					log.Errorf("写入音频数据失败: %v", err)
					return
				}
			}
		}
	}()

	// 收集所有的Opus帧
	go func() {
		for frame := range outputChan {
			frames = append(frames, frame)
		}
	}()

	// 等待完成或超时
	select {
	case <-ctx.Done():
		p.returnConnection(wrapper)
		return nil, fmt.Errorf("TTS合成超时或被取消")
	case <-done:
		close(outputChan)
		p.returnConnection(wrapper)
		return frames, nil
	}
}

// TextToSpeechStream 流式语音合成
func (p *EdgeOfflineTTSProvider) TextToSpeechStream(ctx context.Context, text string, sampleRate int, channels int, frameDuration int) (chan []byte, error) {
	outputChan := make(chan []byte, 100)

	go func() {
		// 获取连接
		wrapper, err := p.getConnection(ctx)
		if err != nil {
			log.Errorf("获取WebSocket连接失败: %v", err)
			return
		}

		// 发送文本
		err = wrapper.conn.WriteMessage(websocket.TextMessage, []byte(text))
		if err != nil {
			p.removeConnection(wrapper)
			log.Errorf("发送文本失败: %v", err)
			return
		}

		// 创建管道用于音频数据传输
		pipeReader, pipeWriter := io.Pipe()

		defer func() {
			pipeReader.Close()
			pipeWriter.Close()
		}()

		// 启动解码器
		go func() {
			startTs := time.Now().UnixMilli()
			// 创建音频解码器
			audioDecoder, err := common.CreateAudioDecoder(ctx, pipeReader, outputChan, frameDuration, "pcm")
			if err != nil {
				pipeReader.Close()
				log.Errorf("创建音频解码器失败: %v", err)
				return
			}

			audioDecoder.WithFormat(beep.Format{
				SampleRate:  beep.SampleRate(sampleRate),
				NumChannels: channels,
				Precision:   2,
			})

			if err := audioDecoder.Run(startTs); err != nil {
				log.Errorf("音频解码失败: %v", err)
			}
		}()

		// 接收WebSocket数据并写入管道
		for {
			select {
			case <-ctx.Done():
				p.returnConnection(wrapper)
				return
			default:
				messageType, data, err := wrapper.conn.ReadMessage()
				if err != nil {
					if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
						p.returnConnection(wrapper)
						return
					}
					log.Errorf("读取WebSocket消息失败: %v", err)
					p.removeConnection(wrapper)
					return
				}

				if messageType == websocket.BinaryMessage {
					if _, err := pipeWriter.Write(data); err != nil {
						log.Errorf("写入音频数据失败: %v", err)
						p.removeConnection(wrapper)
						return
					}
					p.returnConnection(wrapper)
					return
				}
			}
		}
	}()

	return outputChan, nil
}
