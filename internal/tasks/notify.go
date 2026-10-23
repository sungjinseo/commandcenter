package tasks

import (
	"context"
	"fmt"
	"strings"
	"time"

	"commandcenter/internal/logsafe"
	"commandcenter/internal/meta"
)

// mailLogLines is how many of the last run-log lines a failure mail carries.
const mailLogLines = 20

// notifyFailure queues one e-mail for a failed run of t when the task has
// recipients and e-mail is enabled. It never blocks the run: the mail goes to
// the async sender, and anything that goes wrong here is logged only.
func (s *Service) notifyFailure(t meta.Task, out Outcome, rlog *RunLog) {
	if s.Mail == nil || len(t.NotifyEmail) == 0 {
		return
	}
	if s.baseContext().Err() != nil {
		return // the failure is the server shutting down
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.baseContext()), 10*time.Second)
	defer cancel()
	set, err := s.DB.LoadSettings(ctx)
	if err != nil {
		s.log().Warn("could not read the e-mail settings for a failed task", "task", t.ID, "err", logsafe.Err(err))
		return
	}
	if !set.SMTP.Enabled {
		return
	}
	start := out.Start.In(s.loc())
	subject := failureSubject(set.DefaultLanguage, t.Name)
	body := failureBody(set.DefaultLanguage, t.Name, start, out.Message, rlog.String())
	s.Mail.SendAsync(set.SMTP, t.NotifyEmail, subject, body)
}

func failureSubject(lang, name string) string {
	if lang == "en" {
		return fmt.Sprintf("[Command Center] Task failed: %s", name)
	}
	return fmt.Sprintf("[Command Center] 작업 실패: %s", name)
}

// failureBody composes the mail text in lang ("ko" or "en"). log is the run
// log, already redacted; the last mailLogLines lines are included.
func failureBody(lang, name string, at time.Time, message, log string) string {
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	if len(lines) > mailLogLines {
		lines = lines[len(lines)-mailLogLines:]
	}
	when := at.Format("2006-01-02 15:04:05 MST")
	var b strings.Builder
	if lang == "en" {
		fmt.Fprintf(&b, "The scheduled task %q failed.\n\nTime: %s\nMessage: %s\n\nLast %d log lines:\n", name, when, message, mailLogLines)
	} else {
		fmt.Fprintf(&b, "예약 작업 %q 이(가) 실패했습니다.\n\n시각: %s\n메시지: %s\n\n마지막 로그 %d줄:\n", name, when, message, mailLogLines)
	}
	b.WriteString(strings.Join(lines, "\n"))
	b.WriteString("\n")
	return b.String()
}
