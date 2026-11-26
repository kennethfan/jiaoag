package events

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/kennethfan/jiaoag/internal/domain"
	"github.com/kennethfan/jiaoag/internal/protocol"
	"github.com/kennethfan/jiaoag/internal/store"
)

// localPollInterval 是直连模式下的事件轮询间隔，与跨运行时的事件 tail 同语义：
// 读的人不知道写的人是谁，只能按 seq 增量查库。
const localPollInterval = 250 * time.Millisecond

// runLocal 直接读数据库导出事件与审计，不经过服务端。
//
// 为什么值得单独做一条路径：导出的都是只读查询，走 WS 要先拉起一个服务端、
// 拿到令牌、再订阅；而库就在本地。唯一的代价是没有推送，用 250ms 轮询补。
func runLocal(ctx context.Context, db *store.Store, path string, opts options) error {
	sessionID := opts.sessionID
	if sessionID == "" {
		sessions, err := db.ListSessions(ctx)
		if err != nil {
			return err
		}
		if len(sessions) == 0 {
			return fmt.Errorf("数据库 %s 里还没有会话", path)
		}
		sessionID = string(sessions[0].ID)
	}

	if opts.sessionID != "" {
		// 本地直连不做 id 前缀补全：查不到就报错，不要安静导出 0 行。
		if _, err := db.GetSession(ctx, domain.SessionID(opts.sessionID)); err != nil {
			return fmt.Errorf("events: 找不到会话 %s（直接读库只接受完整 id）: %w", opts.sessionID, err)
		}
	}

	seqHead, err := db.LastSeq(ctx, domain.SessionID(sessionID))
	if err != nil {
		return err
	}
	head := int64(seqHead)
	auditHead, err := db.AuditHead(ctx)
	if err != nil {
		return err
	}

	// -1 表示「只看新数据」：事件从当前 head 起，审计从当前 head 起。
	last := opts.from
	if last < 0 {
		last = head
	}
	if head < last {
		head = last
	}
	cursor := opts.auditFrom
	if cursor < 0 {
		cursor = auditHead
	}
	if auditHead < cursor {
		auditHead = cursor
	}

	fmt.Fprintf(os.Stderr, "直接读取 %s\n", path)
	fmt.Fprintf(os.Stderr, "会话 %s，事件从 seq=%d 起，审计从 id=%d 起\n", sessionID, last, cursor)

	out := &printer{format: opts.format}
	for {
		// 事件：按 seq 分批追到当前末尾。中途写入的会被下一轮 250ms 再拉。
		for {
			events, err := db.Replay(ctx, domain.SessionID(sessionID), domain.Seq(last), 500)
			if err != nil {
				return err
			}
			for _, ev := range events {
				if ev.Seq == 0 || int64(ev.Seq) <= last {
					continue
				}
				stream := protocol.StreamEventPayload{
					Kind:    string(ev.Type),
					Payload: ev.Payload,
					TaskID:  string(ev.TaskID),
				}
				raw, err := json.Marshal(stream)
				if err != nil {
					continue
				}
				out.event(protocol.Envelope{
					Type:      protocol.TypeEvent,
					SessionID: sessionID,
					Seq:       int64(ev.Seq),
					TS:        ev.At.UnixMilli(),
					Payload:   raw,
				}, stream)
				last = int64(ev.Seq)
			}
			if len(events) < 500 {
				break
			}
		}

		// 审计：全局游标，按 id 递增。
		for {
			records, err := db.ListAudit(ctx, cursor, 200)
			if err != nil {
				return err
			}
			for _, r := range records {
				out.audit(protocol.AuditRecordPayload{
					ID:         r.ID,
					At:         r.At.UnixMilli(),
					Actor:      r.Actor,
					Action:     r.Action,
					TaskID:     string(r.TaskID),
					SessionID:  string(r.SessionID),
					ToolCallID: string(r.ToolCallID),
					Detail:     r.Detail,
				})
				cursor = r.ID
			}
			if len(records) < 200 {
				break
			}
		}

		// -until-now 以启动时刻的 head 为终点：之后新产生的不追。
		if opts.untilNow && last >= head && cursor >= auditHead {
			return nil
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(localPollInterval):
		}
	}
}
