测试小智官方服务器mqtt+udp协议 响应速度
结果:
stt 166ms，llm 300ms左右，首帧音频642ms

## 使用方法

### 基本参数
- `-ota`: OTA服务器地址（默认: https://api.tenclass.net/xiaozhi/ota/）
- `-device`: 设备ID（默认: ba:8f:17:de:94:94）
- `-mode`: 拾音模式，支持 `manual`（手动）或 `auto`（自动），默认: `manual`

### 拾音模式说明
- **manual 模式**：需要手动发送 listen stop 消息来停止拾音
- **auto 模式**：自动检测语音结束并停止拾音

### 使用示例
```bash
# 使用默认的 manual 模式
./main -device "ba:8f:17:de:94:94"

# 使用 auto 模式
./main -device "ba:8f:17:de:94:94" -mode auto
```