package check

// Device 是自检平台对“设备”的最小抽象。
// 不同型号在其上提供自己的检查集，执行器无需关心型号差异。
type Device interface {
	ID() string
	Model() string
}
