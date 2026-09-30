// Package gatherup 提供社区活动的整组报名、候补与临时占位管理。
// 数据保存在进程内存，每个 Activity 实例管理一个活动。
//
// 所有操作都携带调用方给定的非递减整数秒时间。每次调用时，
// 先把该时刻之前（含该时刻）的全部到期与递补处理完，再处理本次外部操作。
package gatherup

import "errors"

// PendingTTL 是待确认占位从实际获席时起持续的秒数。
const PendingTTL int64 = 900

// Status 表示一笔报名的状态。
type Status int

const (
	StatusWaitlisted Status = iota // 候补
	StatusPending                  // 待确认（占用席位）
	StatusConfirmed                // 已确认（占用席位）
	StatusCancelled                // 已取消
)

// String 返回状态的可读名称。
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
	default:
		return "unknown"
	}
}

// 各类错误。
var (
	ErrInvalidCapacity = errors.New("gatherup: capacity must be positive")
	ErrInvalidSize     = errors.New("gatherup: group size must be positive and not exceed capacity")
	ErrNotFound        = errors.New("gatherup: registration not found")
	ErrNotPending      = errors.New("gatherup: registration is not pending confirmation")
	ErrNotCancellable  = errors.New("gatherup: registration is already cancelled")
)

// RegistrationView 是一笔报名的对外快照。
type RegistrationView struct {
	ID          int    // 报名编号，按报名顺序从 1 开始
	Size        int    // 整组人数
	Status      Status // 当前状态
	GrantedAt   int64  // 获席（转为待确认）的时刻，未获席为 -1
	CancelledAt int64  // 取消生效时刻，未取消为 -1
}

// View 是某时刻活动状态的对外快照。
type View struct {
	Capacity      int
	Remaining     int // 剩余席位
	Registrations []RegistrationView
}

// Activity 管理一个活动的全部报名。非并发安全。
type Activity struct {
	capacity int
	lastTime int64
	regs     []*registration // 按报名先后顺序排列，顺序不可变
}

// registration 是内部报名记录。
type registration struct {
	id          int
	size        int
	status      Status
	grantedAt   int64
	cancelledAt int64
}

// NewActivity 创建一个容量为 capacity 的活动实例。
func NewActivity(capacity int) *Activity {
	if capacity <= 0 {
		panic(ErrInvalidCapacity)
	}
	return &Activity{capacity: capacity}
}

// Register 以整组 size 人报名，返回报名编号。
// 报名立即进入候补，并按同一规则参与本次调用内的递补。
func (a *Activity) Register(now int64, size int) (int, error) {
	if size <= 0 || size > a.capacity {
		return 0, ErrInvalidSize
	}
	now = a.advance(now)
	a.regs = append(a.regs, &registration{
		id:          len(a.regs) + 1,
		size:        size,
		status:      StatusWaitlisted,
		grantedAt:   -1,
		cancelledAt: -1,
	})
	a.allocate(now)
	return len(a.regs), nil
}

// Confirm 确认一笔待确认的报名。恰在截止时刻确认不能成功，
// 因为该时刻的到期释放先于外部操作生效。
func (a *Activity) Confirm(now int64, id int) error {
	now = a.advance(now)
	r := a.find(id)
	if r == nil {
		return ErrNotFound
	}
	if r.status != StatusPending {
		return ErrNotPending
	}
	r.status = StatusConfirmed
	return nil
}

// Cancel 取消一笔报名：释放其占用的全部席位，或使其退出候补。
// 释放席位造成的新分配在本次调用内完成。
func (a *Activity) Cancel(now int64, id int) error {
	now = a.advance(now)
	r := a.find(id)
	if r == nil {
		return ErrNotFound
	}
	if r.status == StatusCancelled {
		return ErrNotCancellable
	}
	r.status = StatusCancelled
	r.cancelledAt = now
	a.allocate(now)
	return nil
}

// View 返回 now 时刻的状态快照。
func (a *Activity) View(now int64) View {
	now = a.advance(now)
	v := View{
		Capacity:      a.capacity,
		Remaining:     a.capacity - a.used(),
		Registrations: make([]RegistrationView, 0, len(a.regs)),
	}
	for _, r := range a.regs {
		v.Registrations = append(v.Registrations, RegistrationView{
			ID:          r.id,
			Size:        r.size,
			Status:      r.status,
			GrantedAt:   r.grantedAt,
			CancelledAt: r.cancelledAt,
		})
	}
	return v
}

// advance 把到 now 为止的全部到期与递补处理完，返回钳制后的时刻。
// 同一时刻到期的报名先一起释放席位，再以该时刻为获席时间递补；
// 空档中新获席者若再次到期，会在后续轮次继续处理。
func (a *Activity) advance(now int64) int64 {
	if now < a.lastTime {
		now = a.lastTime
	}
	for {
		expiry, found := a.earliestExpiry(now)
		if !found {
			break
		}
		for _, r := range a.regs {
			if r.status == StatusPending && r.grantedAt+PendingTTL == expiry {
				r.status = StatusCancelled
				r.cancelledAt = expiry
			}
		}
		a.allocate(expiry)
	}
	a.lastTime = now
	return now
}

// earliestExpiry 返回不超过 limit 的最早待确认截止时刻。
func (a *Activity) earliestExpiry(limit int64) (int64, bool) {
	var earliest int64
	found := false
	for _, r := range a.regs {
		if r.status != StatusPending {
			continue
		}
		expiry := r.grantedAt + PendingTTL
		if expiry <= limit && (!found || expiry < earliest) {
			earliest = expiry
			found = true
		}
	}
	return earliest, found
}

// allocate 在 t 时刻重复选择人数不超过剩余席位的最早候补，
// 整组转为待确认，直到没有任何候补能放下。
func (a *Activity) allocate(t int64) {
	for {
		remaining := a.capacity - a.used()
		var pick *registration
		for _, r := range a.regs {
			if r.status == StatusWaitlisted && r.size <= remaining {
				pick = r
				break
			}
		}
		if pick == nil {
			return
		}
		pick.status = StatusPending
		pick.grantedAt = t
	}
}

// used 返回当前被待确认和已确认占用的席位数。
func (a *Activity) used() int {
	total := 0
	for _, r := range a.regs {
		if r.status == StatusPending || r.status == StatusConfirmed {
			total += r.size
		}
	}
	return total
}

// find 按编号查找报名。
func (a *Activity) find(id int) *registration {
	for _, r := range a.regs {
		if r.id == id {
			return r
		}
	}
	return nil
}
