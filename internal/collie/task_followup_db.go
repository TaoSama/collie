package collie

import (
	"encoding/json"
)

func (sp *Super) saveTaskLocked(task *TaskFollowup, trigger *Trigger, thread *Thread, decision *Entry) error {
	data, err := json.Marshal(task)
	if err != nil {
		return err
	}
	tx, err := sp.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO task_followups(path,data) VALUES(?,?)
		ON CONFLICT(path) DO UPDATE SET data=excluded.data`, task.Path, string(data)); err != nil {
		return err
	}
	if err := sp.persistTriggerLocked(tx, trigger, thread); err != nil {
		return err
	}
	if decision != nil {
		if err := writeStateSeq(tx, sp.st.Seq); err != nil {
			return err
		}
		if err := writeEntry(tx, decision); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if sp.st.TaskFollowups == nil {
		sp.st.TaskFollowups = map[string]*TaskFollowup{}
	}
	sp.st.TaskFollowups[task.Path] = task
	if trigger != nil {
		sp.publishTriggerLocked(trigger, thread)
	} else {
		sp.notifySubscribersLocked()
	}
	return nil
}
