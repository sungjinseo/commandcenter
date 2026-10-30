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

// notifyRun queues one e-mail for a run of t when the task has recipients,
// e-mail is enabled and the run is one to mail: a failed run, or with
// NotifySuccessFailure a finished one too (a stopped run never). It never
// blocks the run: the mail goes to the async sender, and anything that
// goes wrong here is logged only.
func (s *Service) notifyRun(t meta.Task, out Outcome, rlog *RunLog) {
	if s.Mail == nil || len(t.NotifyEmail) == 0 {
		return
	}
	ok := out.Result == ResultOK
	if !ok && out.Result != ResultFailed || ok && t.NotifyOn != NotifySuccessFailure {
		return
	}
	if s.baseContext().Err() != nil {
		return // the failure is the server shutting down
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.baseContext()), 10*time.Second)
	defer cancel()
	set, err := s.DB.LoadSettings(ctx)
	if err != nil {
		s.log().Warn("could not read the e-mail settings for a task run", "task", t.ID, "err", logsafe.Err(err))
		return
	}
	if !set.SMTP.Enabled {
		return
	}
	start := out.Start.In(s.loc())
	subject := failureSubject(set.DefaultLanguage, t.Name)
	body := failureBody(set.DefaultLanguage, t.Name, start, out.Message, rlog.String())
	if ok {
		subject = successSubject(set.DefaultLanguage, t.Name)
		body = successBody(set.DefaultLanguage, t.Name, start, out.Message, rlog.String())
	}
	s.Mail.SendAsync(set.SMTP, t.NotifyEmail, subject, body)
}

func failureSubject(lang, name string) string {
	if lang == "en" {
		return fmt.Sprintf("[Command Center] Task failed: %s", name)
	}
	return fmt.Sprintf("[Command Center] 작업 실패: %s", name)
}

func successSubject(lang, name string) string {
	if lang == "en" {
		return fmt.Sprintf("[Command Center] Task completed: %s", name)
	}
	return fmt.Sprintf("[Command Center] 작업 완료: %s", name)
}

// failureBody composes the mail text in lang ("ko" or "en"). log is the run
// log, already redacted; the last mailLogLines lines are included.
func failureBody(lang, name string, at time.Time, message, log string) string {
	if lang == "en" {
		return mailBody(fmt.Sprintf("The scheduled task %q failed.", name), "Time", "Message", "Last %d log lines:", at, message, log)
	}
	return mailBody(fmt.Sprintf("예약 작업 %q 이(가) 실패했습니다.", name), "시각", "메시지", "마지막 로그 %d줄:", at, message, log)
}

// successBody is failureBody for a run that finished.
func successBody(lang, name string, at time.Time, message, log string) string {
	if lang == "en" {
		return mailBody(fmt.Sprintf("The scheduled task %q completed.", name), "Time", "Message", "Last %d log lines:", at, message, log)
	}
	return mailBody(fmt.Sprintf("예약 작업 %q 이(가) 완료되었습니다.", name), "시각", "메시지", "마지막 로그 %d줄:", at, message, log)
}

func mailBody(head, timeLabel, msgLabel, logLabel string, at time.Time, message, log string) string {
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	if len(lines) > mailLogLines {
		lines = lines[len(lines)-mailLogLines:]
	}
	when := at.Format("2006-01-02 15:04:05 MST")
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n%s: %s\n%s: %s\n\n"+logLabel+"\n", head, timeLabel, when, msgLabel, message, mailLogLines)
	b.WriteString(strings.Join(lines, "\n"))
	b.WriteString("\n")
	return b.String()
}
