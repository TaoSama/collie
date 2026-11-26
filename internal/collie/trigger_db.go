package collie

import (
	"database/sql"
	"encoding/json"
	"strconv"
)

func writeStateSeq(tx *sql.Tx, seq int) error {
	_, err := tx.Exec(`INSERT INTO kv(k,v) VALUES('seq',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, strconv.Itoa(seq))
	return err
}

func writeTrigger(tx *sql.Tx, t *Trigger) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO triggers(id,n,at,status,source,thread,data) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET status=excluded.status, data=excluded.data`,
		t.ID, idNum(t.ID), t.At.Unix(), t.Status, t.Source, t.Thread, string(data))
	return err
}

func writeThread(tx *sql.Tx, th *Thread) error {
	data, err := json.Marshal(th)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO threads(id,last_at,data) VALUES(?,?,?)
		ON CONFLICT(id) DO UPDATE SET last_at=excluded.last_at, data=excluded.data`, th.ID, th.LastAt.Unix(), string(data))
	return err
}

func writeEntry(tx *sql.Tx, e *Entry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO entries(id,n,at,thread,data) VALUES(?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET data=excluded.data`, e.ID, idNum(e.ID), e.At.Unix(), e.Thread, string(data))
	return err
}

func writeWorker(tx *sql.Tx, w *Worker) error {
	data, err := json.Marshal(w)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO workers(name,created_at,data) VALUES(?,?,?)
		ON CONFLICT(name) DO UPDATE SET data=excluded.data`, w.Name, w.CreatedAt.Unix(), string(data))
	return err
}

func (sp *Super) persistTriggerLocked(tx *sql.Tx, t *Trigger, th *Thread) error {
	if t == nil {
		return nil
	}
	if err := writeStateSeq(tx, sp.st.Seq); err != nil {
		return err
	}
	if err := writeTrigger(tx, t); err != nil {
		return err
	}
	return writeThread(tx, th)
}

func (sp *Super) ingestDurable(source, thread, from, text, key, target, ref string) (*Trigger, error) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.ingestDurableLocked(source, thread, from, text, key, target, ref)
}

func (sp *Super) ingestDurableLocked(source, thread, from, text, key, target, ref string) (*Trigger, error) {
	t, th := sp.prepareTriggerLocked(source, thread, from, text, key, target, ref)
	if t != nil {
		if err := sp.commitTriggerLocked(t, th, nil, nil); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// commitTriggerLocked publishes the new event only after its related records
// have been committed in the same transaction.
func (sp *Super) commitTriggerLocked(t *Trigger, th *Thread, entry *Entry, worker *Worker) error {
	tx, err := sp.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := sp.persistTriggerLocked(tx, t, th); err != nil {
		return err
	}
	if entry != nil {
		if err := writeEntry(tx, entry); err != nil {
			return err
		}
	}
	if worker != nil {
		if err := writeWorker(tx, worker); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	sp.publishTriggerLocked(t, th)
	if entry != nil {
		sp.appendEntryLocked(entry)
	}
	return nil
}
