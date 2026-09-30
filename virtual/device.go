// Package virtual 为设备自检平台提供可控的虚拟设备，用于演示与测试。
//
// 虚拟设备的每个传感器可以独立配置为以下故障形态：
//   - 传感器不存在：Read 返回 ErrSensorMissing（设备故障，检查记为 error/sensor_missing）；
//   - 读取报错：Read 返回 ErrSensorFault（检查自身无法完成，记为 error/read_error）；
//   - 读数越界：Read 正常返回但数值超出检查阈值（业务不达标，记为 failed/out_of_range）；
//   - 慢响应：Read 超过 ctx 时限，或在 ctx 取消后及时退出（timeout/canceled）。
package virtual

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// ErrSensorMissing 表示设备上根本没有该传感器（未安装/地址不存在）。
var ErrSensorMissing = errors.New("sensor missing")

// ErrSensorFault 表示传感器存在，但读取过程自身报错。
var ErrSensorFault = errors.New("sensor fault")

// Sensor 是一个可控的虚拟传感器。零值不可直接使用，请通过 NewSensor 构造。
type Sensor struct {
	name string

	mu       sync.Mutex
	missing  bool
	fault    bool
	value    float64
	min, max float64 // 该传感器的物理量合理范围，仅用于演示参考
	delay    time.Duration
	inFlight int32
	total    int64
	opened   int64
	closed   int64
}

// NewSensor 构造一个正常读数的虚拟传感器。
func NewSensor(name string, value, min, max float64) *Sensor {
	return &Sensor{name: name, value: value, min: min, max: max}
}

// SetMissing 将传感器置为“设备上不存在”。
func (s *Sensor) SetMissing(missing bool) { s.mu.Lock(); s.missing = missing; s.mu.Unlock() }

// SetFault 将传感器置为“读取自身报错”，并恢复后可再次置回正常。
func (s *Sensor) SetFault(fault bool) { s.mu.Lock(); s.fault = fault; s.mu.Unlock() }

// SetValue 更新正常返回的读数（用于“恢复”场景：先越界，后回到合理区间）。
func (s *Sensor) SetValue(v float64) { s.mu.Lock(); s.value = v; s.mu.Unlock() }

// SetDelay 设置读取延迟。delay>0 时 Read 会等待该时长或 ctx 结束，
// 用于模拟慢响应；ctx 取消/超时后 Read 立即返回 ctx.Err()，不泄漏 goroutine。
func (s *Sensor) SetDelay(d time.Duration) { s.mu.Lock(); s.delay = d; s.mu.Unlock() }

// InFlight 返回当前尚未返回的读取调用数，用于断言资源已收尾。
func (s *Sensor) InFlight() int { return int(atomic.LoadInt32(&s.inFlight)) }

// Stats 返回累计读取次数与打开/关闭（获取/释放）次数。
func (s *Sensor) Stats() (reads int64, opened int64, closed int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total, s.opened, s.closed
}

func (s *Sensor) snapshot() (missing, fault bool, value float64, delay time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.missing, s.fault, s.value, s.delay
}

// acquire 模拟获取传感器句柄（串口/通道），与 release 配对统计收尾。
func (s *Sensor) acquire() bool {
	s.mu.Lock()
	s.opened++
	s.mu.Unlock()
	if !atomic.CompareAndSwapInt32(&s.inFlight, 0, 1) {
		panic("virtual: 传感器句柄被并发占用，前次读取未释放")
	}
	return true
}

func (s *Sensor) release() {
	s.mu.Lock()
	s.closed++
	s.mu.Unlock()
	atomic.AddInt32(&s.inFlight, -1)
}

// Read 执行一次传感器读取，严格尊重 ctx 的取消与超时。
func (s *Sensor) Read(ctx context.Context) (float64, error) {
	s.acquire()
	defer s.release()
	s.mu.Lock()
	s.total++
	s.mu.Unlock()

	missing, fault, value, delay := s.snapshot()
	if missing {
		return 0, fmt.Errorf("%w: %s", ErrSensorMissing, s.name)
	}
	if delay > 0 {
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-t.C:
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if fault {
		return 0, fmt.Errorf("%w: %s", ErrSensorFault, s.name)
	}
	return value, nil
}

// Device 是一台由若干命名传感器组成的虚拟设备。
type Device struct {
	id, model string
	sensors   map[string]*Sensor
}

// NewDevice 构造虚拟设备。
func NewDevice(id, model string, sensors map[string]*Sensor) *Device {
	return &Device{id: id, model: model, sensors: sensors}
}

func (d *Device) ID() string    { return d.id }
func (d *Device) Model() string { return d.model }

// Sensor 按名称获取传感器；不存在时返回 nil, false（对应“传感器不存在”）。
func (d *Device) Sensor(name string) (*Sensor, bool) {
	s, ok := d.sensors[name]
	return s, ok
}

// ReadSensor 读取指定传感器，并把三类问题用不同错误区分开：
// ErrSensorMissing（设备无此传感器）、ctx 超时/取消、ErrSensorFault（读取报错）。
func (d *Device) ReadSensor(ctx context.Context, name string) (float64, error) {
	s, ok := d.sensors[name]
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrSensorMissing, name)
	}
	return s.Read(ctx)
}

// Close 等待并断言所有在途读取均已收尾。
func (d *Device) Close() error {
	for name, s := range d.sensors {
		s.mu.Lock()
		opened, closed := s.opened, s.closed
		s.mu.Unlock()
		if s.InFlight() != 0 {
			return fmt.Errorf("传感器 %s 仍有 %d 次读取未结束", name, s.InFlight())
		}
		if opened != closed {
			return fmt.Errorf("传感器 %s 句柄未配对: open=%d close=%d", name, opened, closed)
		}
	}
	return nil
}
