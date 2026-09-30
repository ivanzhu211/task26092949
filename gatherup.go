// Package gatherup 提供社区活动整组报名、候补与临时占位的进程内管理。
//
// 每个 Activity 实例管理一个容量固定的活动。报名按整组人数申请席位，
// 整组不可拆分。所有操作携带调用方给定的非递减整数秒时间；
// 库内部按时间顺序重放到期与递补，因此连续查看与隔很久查看结果一致。
package gatherup

import "errors"

// HoldSeconds 是待确认占位从实际获席时起持续的秒数。
const HoldSeconds int64 = 900

// Status 表示一笔报名的状态。
type Status int

const (
	// StatusWaitlisted 表示报名在候补队列中，不占用席位。
	StatusWaitlisted Status = iota
	// StatusPending 表示报名已获席，处于待确认占位中，占用席位。
	StatusPending
	// StatusConfirmed 表示报名已确认，占用席位且不再自动过期。
	StatusConfirmed
	// StatusCancelled 表示报名已取消，不占用席位。
	StatusCancelled
)

func (s Status) String() string {
	switch s {
	case StatusWaitlisted:
		return "waitlisted"
	case StatusPending:
		return "pending"
	case StatusConfirmed:
		return "confirmed"
	case StatusCancelled:
		return "cancelled"
	}
	return "unknown"
}

// 错误定义。
var (
	ErrInvalidSize      = errors.New("gatherup: group size must be positive")
	ErrInvalidCapacity  = errors.New("gatherup: capacity must be positive")
	ErrUnknownID        = errors.New("gatherup: unknown registration id")
	ErrNotPending       = errors.New("gatherup: registration is not pending confirmation")
	ErrNotCancellable   = errors.New("gatherup: registration is already cancelled")
	ErrNonMonotonicTime = errors.New("gatherup: time must be non-decreasing")
)

// Registration 是一笔报名的内部记录。
type Registration struct {
	ID          int
	Size        int
	Status      Status
	GrantedAt   int64 // 获席时间，未获席为 -1
	CancelledAt int64 // 取消生效时间，未取消为 -1
}

// RegistrationView 是一笔报名的只读视图。
type RegistrationView struct {
	ID          int
	Size        int
	Status      Status
	GrantedAt   int64 // 获席时间，未获席为 -1
	CancelledAt int64 // 取消生效时间，未取消为 -1
}

// View 是活动在某时刻的只读快照。
type View struct {
	Capacity      int
	Remaining     int // 剩余席位（仅待确认与已确认占用席位）
	Registrations []RegistrationView
}

// Activity 管理一个活动的报名、占位与候补。不是并发安全的。
type Activity struct {
	capacity int
	used     int
	now      int64
	started  bool
	nextID   int
	regs     []*Registration
}

// NewActivity 创建一个容量固定的活动实例。
func NewActivity(capacity int) (*Activity, error) {
	if capacity <= 0 {
		return nil, ErrInvalidCapacity
	}
	return &Activity{capacity: capacity, nextID: 1}, nil
}

// Register 以整组人数 size 在时刻 now 报名，返回报名 ID。
// 报名进入候补队列后立即按规则尝试分配。
func (a *Activity) Register(now int64, size int) (int, error) {
	if size <= 0 {
		return 0, ErrInvalidSize
	}
	if err := a.checkTime(now); err != nil {
		return 0, err
	}
	a.advance(now)
	id := a.nextID
	a.nextID++
	a.regs = append(a.regs, &Registration{
		ID: id, Size: size, Status: StatusWaitlisted,
		GrantedAt: -1, CancelledAt: -1,
	})
	a.allocate(now)
	return id, nil
}

// Confirm 在时刻 now 确认一笔待确认报名。确认后不再自动过期。
// 恰在截止时刻确认不能成功：到期释放先于外部操作生效。
func (a *Activity) Confirm(now int64, id int) error {
	if err := a.checkTime(now); err != nil {
		return err
	}
	a.advance(now)
	r := a.find(id)
	if r == nil {
		return ErrUnknownID
	}
	if r.Status != StatusPending {
		return ErrNotPending
	}
	r.Status = StatusConfirmed
	return nil
}

// Cancel 在时刻 now 取消一笔报名：释放其占用的全部席位，
// 或使其退出候补。释放席位后立即按规则递补。
func (a *Activity) Cancel(now int64, id int) error {
	if err := a.checkTime(now); err != nil {
		return err
	}
	a.advance(now)
	r := a.find(id)
	if r == nil {
		return ErrUnknownID
	}
	if r.Status == StatusCancelled {
		return ErrNotCancellable
	}
	if r.Status == StatusPending || r.Status == StatusConfirmed {
		a.used -= r.Size
	}
	r.Status = StatusCancelled
	r.CancelledAt = now
	a.allocate(now)
	return nil
}

// Status 推进到时刻 now 并返回活动快照。
// 快照包含剩余席位与每笔报名的状态、获席时间和取消生效时间。
func (a *Activity) Status(now int64) View {
	a.advance(now)
	v := View{Capacity: a.capacity, Remaining: a.capacity - a.used}
	for _, r := range a.regs {
		v.Registrations = append(v.Registrations, RegistrationView{
			ID:          r.ID,
			Size:        r.Size,
			Status:      r.Status,
			GrantedAt:   r.GrantedAt,
			CancelledAt: r.CancelledAt,
		})
	}
	return v
}

// checkTime 校验外部操作时间非递减。
func (a *Activity) checkTime(now int64) error {
	if a.started && now < a.now {
		return ErrNonMonotonicTime
	}
	return nil
}

// find 按 ID 查找报名。
func (a *Activity) find(id int) *Registration {
	for _, r := range a.regs {
		if r.ID == id {
			return r
		}
	}
	return nil
}

// advance 把内部时间推进到 now：按截止时刻从早到晚逐轮处理，
// 每轮先让该时刻全部到期的待确认报名一起取消并释放席位，
// 再以该时刻为获席时间递补；空档中新获席者也可能再次到期。
func (a *Activity) advance(now int64) {
	if !a.started {
		a.started = true
		a.now = now
	}
	if now < a.now {
		now = a.now
	}
	for {
		deadline := int64(-1)
		for _, r := range a.regs {
			if r.Status != StatusPending {
				continue
			}
			d := r.GrantedAt + HoldSeconds
			if d <= now && (deadline == -1 || d < deadline) {
				deadline = d
			}
		}
		if deadline == -1 {
			break
		}
		for _, r := range a.regs {
			if r.Status == StatusPending && r.GrantedAt+HoldSeconds == deadline {
				r.Status = StatusCancelled
				r.CancelledAt = deadline
				a.used -= r.Size
			}
		}
		a.allocate(deadline)
	}
	a.now = now
}

// allocate 在时刻 t 重复分配：每次从人数不超过剩余席位的候补中
// 选择最早报名者，整组转为待确认，直到没有候补能放下。
// 暂时放不下的大组保留原顺序，不挡住后面能放下的小组。
func (a *Activity) allocate(t int64) {
	for {
		var pick *Registration
		for _, r := range a.regs {
			if r.Status == StatusWaitlisted && r.Size <= a.capacity-a.used {
				pick = r
				break
			}
		}
		if pick == nil {
			return
		}
		pick.Status = StatusPending
		pick.GrantedAt = t
		a.used += pick.Size
	}
}
