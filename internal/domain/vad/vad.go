package vad

import (
	"errors"
	"fmt"

	"sync"
	"time"

	"github.com/maxhawkins/go-webrtcvad" // 替换为WebRTC VAD包
	"github.com/spf13/viper"
)

// VAD默认配置 - 更新配置项
var defaultVADConfig = map[string]interface{}{
	"mode":                    2, // WebRTC VAD模式 (0-3)
	"min_silence_duration_ms": int64(100),
	"sample_rate":             16000, // 采样率必须为8k/16k/32k/48k
	"channels":                1,     // 仅支持单声道
	"frame_duration_ms":       20,    // 帧时长(ms)
}

// 资源池默认配置
var defaultPoolConfig = struct {
	MaxSize        int
	AcquireTimeout int64
}{
	MaxSize:        10,
	AcquireTimeout: 3000,
}

// 配置项路径
const (
	ConfigKeyVADMode         = "vad.mode"      // 新增模式配置
	ConfigKeyVADThreshold    = "vad.threshold" // 不再使用
	ConfigKeySilenceDuration = "vad.min_silence_duration_ms"
	ConfigKeySampleRate      = "vad.sample_rate"
	ConfigKeyChannels        = "vad.channels"
	ConfigKeyFrameDuration   = "vad.frame_duration_ms" // 新增帧时长配置
	ConfigKeyPoolSize        = "vad.pool_size"
	ConfigKeyAcquireTimeout  = "vad.acquire_timeout_ms"
	ConfigKeyVADModelPath    = "vad.model_path" // 不再需要，但保留避免配置错误
)

// 全局变量
var (
	opusDecoderMap        sync.Map
	vadDetectorMap        sync.Map
	initMutex             sync.Mutex
	initialized           = false
	globalVADResourcePool *VADResourcePool
	vadFactoryFunc        func(string, map[string]interface{}) (VAD, error)
)

// 初始化VAD模块
func InitVAD() error {
	fmt.Printf("VAD模块初始化...")

	globalVADResourcePool = &VADResourcePool{
		maxSize:        defaultPoolConfig.MaxSize,
		acquireTimeout: defaultPoolConfig.AcquireTimeout,
		defaultConfig:  defaultVADConfig,
		initialized:    false,
	}

	vadFactoryFunc = createVADInstance

	err := InitVADFromConfig()
	if err != nil {
		fmt.Printf("VAD模块初始化失败: %v", err)
		return err
	}

	fmt.Printf("VAD模块初始化完成")
	return nil
}

// CreateVAD 创建指定类型的VAD实例（公共API）
func CreateVAD(vadType string, config map[string]interface{}) (VAD, error) {
	return vadFactoryFunc(vadType, config)
}

// 从配置初始化
func InitVADFromConfig() error {
	// 不再需要模型路径，但保留配置项兼容性
	modelPath := viper.GetString(ConfigKeyVADModelPath)
	if modelPath == "" {
		modelPath = "webrtc" // 填充虚拟值
	}

	// 更新配置
	if mode := viper.GetInt(ConfigKeyVADMode); mode >= 0 && mode <= 3 {
		globalVADResourcePool.defaultConfig["mode"] = mode
	}

	if silenceMs := viper.GetInt64(ConfigKeySilenceDuration); silenceMs > 0 {
		globalVADResourcePool.defaultConfig["min_silence_duration_ms"] = silenceMs
	}

	if sampleRate := viper.GetInt(ConfigKeySampleRate); sampleRate > 0 {
		globalVADResourcePool.defaultConfig["sample_rate"] = sampleRate
	}

	if frameDur := viper.GetInt(ConfigKeyFrameDuration); frameDur > 0 {
		globalVADResourcePool.defaultConfig["frame_duration_ms"] = frameDur
	}

	if channels := viper.GetInt(ConfigKeyChannels); channels > 0 {
		globalVADResourcePool.defaultConfig["channels"] = channels
	}

	if poolSize := viper.GetInt(ConfigKeyPoolSize); poolSize > 0 {
		globalVADResourcePool.maxSize = poolSize
	}

	if timeout := viper.GetInt64(ConfigKeyAcquireTimeout); timeout > 0 {
		globalVADResourcePool.acquireTimeout = timeout
	}

	// 设置虚拟模型路径
	globalVADResourcePool.defaultConfig["model_path"] = modelPath
	return initVADResourcePool(modelPath)
}

// 初始化资源池
func initVADResourcePool(modelPath string) error {
	initMutex.Lock()
	defer initMutex.Unlock()

	if globalVADResourcePool.initialized {
		return nil
	}

	// 验证配置
	// 删除未使用的 sampleRate 变量声明
	channels := globalVADResourcePool.defaultConfig["channels"].(int)
	if channels != 1 {
		return errors.New("WebRTC VAD仅支持单声道(channels=1)")
	}

	// 初始化资源池
	err := globalVADResourcePool.initialize()
	if err != nil {
		return fmt.Errorf("初始化VAD资源池失败: %v", err)
	}

	globalVADResourcePool.initialized = true
	fmt.Printf("VAD资源池初始化完成，池大小: %d", globalVADResourcePool.maxSize)
	return nil
}

// VAD接口
type VAD interface {
	IsVAD(pcmData []byte) (bool, error)
	Reset() error
	Close() error
}

// WebRTCVAD实现
type WebRTCVAD struct {
	vad        *webrtcvad.VAD
	mode       int
	sampleRate int
	frameSize  int // 每帧样本数
	mu         sync.Mutex
}

// 创建WebRTCVAD实例
func NewWebRTCVAD(config map[string]interface{}) (*WebRTCVAD, error) {
	// 解析配置
	mode, _ := config["mode"].(int)
	sampleRate, _ := config["sample_rate"].(int)
	frameDuration, _ := config["frame_duration_ms"].(int)
	channels, _ := config["channels"].(int)

	// 验证参数
	if channels != 1 {
		return nil, errors.New("WebRTC VAD仅支持单声道")
	}

	// 计算帧大小
	frameSize := (sampleRate * frameDuration) / 1000

	// 创建VAD实例
	vad, err := webrtcvad.New()
	if err != nil {
		return nil, err
	}

	// 设置模式
	if err := vad.SetMode(mode); err != nil {
		return nil, err
	}

	// 验证帧参数
	if ok := vad.ValidRateAndFrameLength(sampleRate, frameSize); !ok {
		return nil, fmt.Errorf("无效的采样率或帧长度: rate=%d, frame=%d", sampleRate, frameSize)
	}

	return &WebRTCVAD{
		vad:        vad,
		mode:       mode,
		sampleRate: sampleRate,
		frameSize:  frameSize,
	}, nil
}

// 检测语音活动// 检测语音活动
func (w *WebRTCVAD) IsVAD(pcmData []byte) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// 直接处理字节数据
	active, err := w.vad.Process(w.sampleRate, pcmData)
	if err != nil {
		return false, err
	}
	return active, nil
}

// 辅助函数：float32转int16
func float32ToInt16(data []float32) []int16 {
	int16Data := make([]int16, len(data))
	for i, v := range data {
		// 范围转换 [-1.0, 1.0] -> [-32768, 32767]
		val := v * 32768.0
		if val > 32767.0 {
			val = 32767.0
		} else if val < -32768.0 {
			val = -32768.0
		}
		int16Data[i] = int16(val)
	}
	return int16Data
}

// 辅助函数：int16转小端字节
func int16ToBytes(data []int16) []byte {
	buf := make([]byte, len(data)*2)
	for i, v := range data {
		buf[i*2] = byte(v)
		buf[i*2+1] = byte(v >> 8)
	}
	return buf
}

func (w *WebRTCVAD) Reset() error {
	// WebRTC VAD无状态，无需重置
	return nil
}

func (w *WebRTCVAD) Close() error {
	// go-webrtcvad不需要显式关闭
	return nil
}

// 工厂函数
func createVADInstance(vadType string, config map[string]interface{}) (VAD, error) {
	switch vadType {

	case "WebRTCVAD":
		return NewWebRTCVAD(config)
	default:
		return nil, errors.New("不支持的VAD类型: " + vadType)
	}
}

// VAD资源池（保持原有逻辑不变）
type VADResourcePool struct {
	availableVADs  chan VAD
	allocatedVADs  sync.Map
	maxSize        int
	acquireTimeout int64
	defaultConfig  map[string]interface{}
	mu             sync.Mutex
	initialized    bool
}

func (p *VADResourcePool) initialize() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 清理现有资源
	if p.availableVADs != nil {
		close(p.availableVADs)
		for vad := range p.availableVADs {
			vad.Close()
		}
	}

	// 创建新资源池
	p.availableVADs = make(chan VAD, p.maxSize)

	// 预创建实例
	for i := 0; i < p.maxSize; i++ {
		vad, err := CreateVAD("WebRTCVAD", p.defaultConfig)
		if err != nil {
			// 清理已创建实例
			for j := 0; j < i; j++ {
				v := <-p.availableVADs
				v.Close()
			}
			return fmt.Errorf("创建VAD实例失败: %v", err)
		}
		p.availableVADs <- vad
	}

	return nil
}

// AcquireVAD 从资源池获取一个VAD实例
func (p *VADResourcePool) AcquireVAD() (VAD, error) {
	if !p.initialized {
		return nil, errors.New("VAD资源池未初始化")
	}

	// 设置超时
	timeout := time.After(time.Duration(p.acquireTimeout) * time.Millisecond)

	fmt.Printf("获取VAD实例, 当前可用: %d/%d", len(p.availableVADs), p.maxSize)

	// 尝试从池中获取一个VAD实例
	select {
	case vad := <-p.availableVADs:
		if vad == nil {
			return nil, errors.New("VAD资源池已关闭")
		}

		// 标记为已分配
		p.allocatedVADs.Store(vad, time.Now())

		fmt.Printf("从VAD资源池获取了一个VAD实例，当前可用: %d/%d", len(p.availableVADs), p.maxSize)
		return vad, nil

	case <-timeout:
		return nil, fmt.Errorf("获取VAD实例超时，当前资源池已满载运行（%d/%d）", p.maxSize, p.maxSize)
	}
}

// ReleaseVAD 释放VAD实例回资源池
func (p *VADResourcePool) ReleaseVAD(vad VAD) {
	if vad == nil || !p.initialized {
		return
	}

	fmt.Printf("释放VAD实例: %v, 当前可用: %d/%d", vad, len(p.availableVADs), p.maxSize)

	// 检查是否是从此池分配的实例
	if _, exists := p.allocatedVADs.Load(vad); exists {
		// 从已分配映射中删除
		p.allocatedVADs.Delete(vad)

		// 如果资源池已关闭，直接销毁实例
		if p.availableVADs == nil {
			if sileroVAD, ok := vad.(*WebRTCVAD); ok {
				sileroVAD.Close()
			}
			return
		}

		// 尝试放回资源池，如果满了就丢弃
		select {
		case p.availableVADs <- vad:
			fmt.Printf("VAD实例已归还资源池，当前可用: %d/%d", len(p.availableVADs), p.maxSize)
		default:
			// 资源池满了，直接关闭实例
			if sileroVAD, ok := vad.(*WebRTCVAD); ok {
				sileroVAD.Close()
			}
			fmt.Printf("VAD资源池已满，多余实例已销毁")
		}
	} else {
		fmt.Printf("尝试释放非此资源池管理的VAD实例")
	}
}

// GetActiveCount 获取当前活跃（被分配）的VAD实例数量
func (p *VADResourcePool) GetActiveCount() int {
	count := 0
	p.allocatedVADs.Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	return count
}

// GetAvailableCount 获取当前可用的VAD实例数量
func (p *VADResourcePool) GetAvailableCount() int {
	if p.availableVADs == nil {
		return 0
	}
	return len(p.availableVADs)
}

// Resize 调整资源池大小
func (p *VADResourcePool) Resize(newSize int) error {
	if newSize <= 0 {
		return errors.New("资源池大小必须大于0")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	currentSize := p.maxSize

	// 如果新大小小于当前大小，需要减少实例数量
	if newSize < currentSize {
		// 更新大小配置
		p.maxSize = newSize

		// 计算需要释放的实例数量
		toRemove := currentSize - newSize
		for i := 0; i < toRemove; i++ {
			// 尝试从可用队列中取出实例并关闭
			select {
			case vad := <-p.availableVADs:
				if sileroVAD, ok := vad.(*WebRTCVAD); ok {
					sileroVAD.Close()
				}
			default:
				// 没有更多可用实例了，退出循环
				break
			}
		}

		fmt.Printf("VAD资源池大小已调整：%d -> %d", currentSize, newSize)
		return nil
	}

	// 如果新大小大于当前大小，需要增加实例数量
	if newSize > currentSize {
		// 计算需要增加的实例数量
		toAdd := newSize - currentSize

		// 创建新的VAD实例
		for i := 0; i < toAdd; i++ {
			vadInstance, err := CreateVAD("WebRTCVAD", p.defaultConfig)
			if err != nil {
				// 有错误发生，更新大小为当前已成功创建的实例数
				actualNewSize := currentSize + i
				p.maxSize = actualNewSize

				fmt.Printf("无法创建全部请求的VAD实例，资源池大小已调整为: %d", actualNewSize)
				return fmt.Errorf("创建新VAD实例失败: %v", err)
			}

			// 放入可用队列
			select {
			case p.availableVADs <- vadInstance:
				// 成功放入队列
			default:
				// 队列已满，直接关闭实例
				if sileroVAD, ok := vadInstance.(*WebRTCVAD); ok {
					sileroVAD.Close()
				}
				fmt.Printf("无法将新创建的VAD实例放入可用队列，实例已销毁")
			}
		}

		// 更新大小配置
		p.maxSize = newSize

		fmt.Printf("VAD资源池大小已调整：%d -> %d", currentSize, newSize)
		return nil
	}

	// 大小相同，无需调整
	return nil
}

// Close 关闭资源池，释放所有资源
func (p *VADResourcePool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.availableVADs != nil {
		// 关闭可用队列
		close(p.availableVADs)

		// 释放所有可用的VAD实例
		for vad := range p.availableVADs {
			if sileroVAD, ok := vad.(*WebRTCVAD); ok {
				sileroVAD.Close()
			}
		}

		p.availableVADs = nil
	}

	// 释放所有已分配的VAD实例
	p.allocatedVADs.Range(func(key, _ interface{}) bool {
		vad := key.(VAD)
		if sileroVAD, ok := vad.(*WebRTCVAD); ok {
			sileroVAD.Close()
		}
		p.allocatedVADs.Delete(key)
		return true
	})

	p.initialized = false
	fmt.Printf("VAD资源池已关闭，所有资源已释放")
}

// GetVADResourcePool 获取全局VAD资源池实例
func GetVADResourcePool() (*VADResourcePool, error) {
	if globalVADResourcePool == nil || !globalVADResourcePool.initialized {
		// 尝试自动初始化
		if err := InitVADFromConfig(); err != nil {
			return nil, errors.New("VAD资源池未完全初始化，请在配置文件中设置 " + ConfigKeyVADModelPath)
		}
	}
	return globalVADResourcePool, nil
}

// AcquireVAD 获取一个VAD实例
func AcquireVAD() (VAD, error) {
	if globalVADResourcePool == nil {
		return nil, errors.New("VAD资源池尚未初始化")
	}

	if !globalVADResourcePool.initialized {
		// 尝试自动初始化
		if err := InitVADFromConfig(); err != nil {
			return nil, errors.New("VAD模型路径未配置，请在配置文件中设置 " + ConfigKeyVADModelPath)
		}
	}

	return globalVADResourcePool.AcquireVAD()
}

// ReleaseVAD 释放一个VAD实例
func ReleaseVAD(vad VAD) {
	if globalVADResourcePool != nil && globalVADResourcePool.initialized {
		globalVADResourcePool.ReleaseVAD(vad)
	}
}

// SetThreshold 设置VAD检测阈值
func (s *WebRTCVAD) SetThreshold(threshold float32) {

}
