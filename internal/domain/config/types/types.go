package types

type AsrConfig struct {
	Provider string                 `json:"provider"`
	Config   map[string]interface{} `json:"config"`
}

type TtsConfig struct {
	Provider string                 `json:"provider"`
	Config   map[string]interface{} `json:"config"`
}

type MemoryConfig struct {
	Provider string                 `json:"provider"`
	Config   map[string]interface{} `json:"config"`
}

type LlmConfig struct {
	Provider string                 `json:"provider"`
	Config   map[string]interface{} `json:"config"`
}

type VadConfig struct {
	Provider string                 `json:"provider"`
	Config   map[string]interface{} `json:"config"`
}

type ConfigItem struct {
	Provider string                 `json:"provider"`
	JsonData map[string]interface{} `json:"json_data"`
}

type SpeakerGroupInfo struct {
	ID          uint     `json:"id"`
	Name        string   `json:"name"`
	Prompt      string   `json:"prompt"`
	Description string   `json:"description"`
	Uuids       []string `json:"uuids"`
	TTSConfigID *string  `json:"tts_config_id"`
	Voice       *string  `json:"voice"`
}

type UConfig struct {
	SystemPrompt  string                      `json:"system_prompt"`
	Asr           AsrConfig                   `json:"asr"`
	Tts           TtsConfig                   `json:"tts"`
	Llm           LlmConfig                   `json:"llm"`
	Vad           VadConfig                   `json:"vad"`
	Memory        MemoryConfig                `json:"memory"`
	VoiceIdentify map[string]SpeakerGroupInfo `json:"voice_identify"` // 声纹识别配置
	AgentId       string                      `json:"agent_id"`       //所属agent_id
}

type TtsConfigItem struct {
	ConfigID  string                 `json:"config_id"`
	Name      string                 `json:"name"`
	Provider  string                 `json:"provider"`
	Config    map[string]interface{} `json:"config"`
	IsDefault bool                   `json:"is_default"`
}
