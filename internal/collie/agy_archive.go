package collie

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"syscall"
	"time"
)

var errAgyCLIArchiveMissing = errors.New("Antigravity CLI 没有这个会话的归档记录")

func (s *Server) agyArchive(ctx context.Context, id string, archived bool) error {
	err := postAgyApp(ctx, agyService+"UpdateConversationAnnotations", map[string]any{
		"cascadeId": id, "mergeAnnotations": true, "annotations": map[string]any{"archived": archived},
	})
	if err == nil || (!agyConversationMissing(err) && !errors.Is(err, errAgyAppClosed)) {
		return err
	}
	if cliErr := s.setAgyCLIArchive(ctx, id, archived); cliErr != nil {
		if errors.Is(cliErr, errAgyCLIArchiveMissing) {
			if errors.Is(err, errAgyAppClosed) {
				return err
			}
			return errors.New("桌面版和 CLI 都找不到这个会话的归档记录")
		}
		return fmt.Errorf("CLI 归档索引没能更新：%w", cliErr)
	}
	return nil
}

func (s *Server) setAgyCLIArchive(ctx context.Context, id string, archived bool) error {
	root := filepath.Join(s.home, ".gemini", "antigravity-cli")
	if !conversationRe.MatchString(id) || !isFile(filepath.Join(root, "conversations", id+".db")) {
		return errAgyCLIArchiveMissing
	}
	path := filepath.Join(root, "conversation_summaries.db")
	if !isFile(path) {
		return errAgyCLIArchiveMissing
	}
	lock, err := lockAgyArchive(root, id)
	if err != nil {
		return err
	}
	defer lock.Close()
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: "mode=rw&_txlock=immediate&_pragma=busy_timeout(2000)"}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	columns, values, err := agyArchiveHeader(ctx, tx, id)
	if err != nil {
		return err
	}
	archiveColumn := slices.Index(columns, "archived")
	if archiveColumn < 0 {
		return errors.New("这个版本的 CLI 索引没有归档字段")
	}
	previous, ok := values[archiveColumn].(int64)
	if !ok || (previous != 0 && previous != 1) {
		return errors.New("这个版本的 CLI 归档字段格式与预期不同")
	}
	annotation, err := readAgyArchiveAnnotation(root, id)
	if err != nil {
		return err
	}
	next, err := annotation.withArchive(archived, previous == 1)
	if err != nil {
		return err
	}
	changeAnnotation := !bytes.Equal(annotation.before, next)
	if (previous == 1) == archived && !changeAnnotation {
		return nil
	}

	entry := make(map[string]any, len(columns))
	for i, name := range columns {
		entry[name] = values[i]
	}
	if err := backupAgyArchiveHeader(path, id, entry, annotation); err != nil {
		return fmt.Errorf("没能备份原索引条目：%w", err)
	}
	if (previous == 1) != archived {
		if err := updateAgyArchiveHeader(ctx, tx, id, entry["rowid"], archived); err != nil {
			return err
		}
	}
	if changeAnnotation {
		if err := os.MkdirAll(filepath.Dir(annotation.path), 0o700); err != nil {
			return err
		}
		if err := replaceArchiveFile(annotation.path, next, annotation.mode); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		if changeAnnotation {
			err = errors.Join(err, annotation.restore())
		}
		return err
	}
	return nil
}

func updateAgyArchiveHeader(ctx context.Context, tx *sql.Tx, id string, row any, archived bool) error {
	value := 0
	if archived {
		value = 1
	}
	result, err := tx.ExecContext(ctx, `UPDATE conversations_fts SET archived = ? WHERE rowid = ? AND conversation_id = ? AND chunk_index = -1`, value, row, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("索引条目已变化，请重试")
	}
	return nil
}

func agyArchiveHeader(ctx context.Context, tx *sql.Tx, id string) ([]string, []any, error) {
	rows, err := tx.QueryContext(ctx, `SELECT rowid, * FROM conversations_fts WHERE conversation_id = ? AND chunk_index = -1 LIMIT 2`, id)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
		return nil, nil, errAgyCLIArchiveMissing
	}
	columns, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	values := make([]any, len(columns))
	dest := make([]any, len(columns))
	for i := range values {
		dest[i] = &values[i]
	}
	if err := rows.Scan(dest...); err != nil {
		return nil, nil, err
	}
	if rows.Next() {
		return nil, nil, errors.New("CLI 索引中有重复的会话条目")
	}
	return columns, values, rows.Err()
}

func backupAgyArchiveHeader(database, id string, entry map[string]any, annotation agyArchiveAnnotation) error {
	dir := filepath.Join(collieDir(), "archive-backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "agy-"+id+"-*.json")
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(map[string]any{
		"database": database, "entry": entry, "annotation_path": annotation.path,
		"annotation_before": annotation.before, "annotation_mode": annotation.mode,
	})
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// The CLI holds this same per-conversation presence lock while it uses the
// session. Hold it through both writes so a running CLI cannot overwrite them.
func lockAgyArchive(root, id string) (*os.File, error) {
	dir := filepath.Join(root, "presence")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, id+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeErr := f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			err = errors.New("会话还在 Antigravity CLI 里打开，请关闭后重试")
		}
		return nil, errors.Join(err, closeErr)
	}
	return f, nil
}

type agyArchiveAnnotation struct {
	path   string
	before []byte
	mode   os.FileMode
}

func readAgyArchiveAnnotation(root, id string) (agyArchiveAnnotation, error) {
	a := agyArchiveAnnotation{path: filepath.Join(root, "annotations", id+".pbtxt"), mode: 0o600}
	info, err := os.Lstat(a.path)
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return a, err
	}
	if !info.Mode().IsRegular() {
		return a, errors.New("CLI 会话注解不是普通文件")
	}
	a.mode = info.Mode().Perm()
	a.before, err = os.ReadFile(a.path)
	return a, err
}

// The CLI writes multiline protobuf text with top-level fields at column
// zero. Keep the other fields byte-for-byte and reject unfamiliar encodings.
var agyArchivedField = regexp.MustCompile(`(?m)^archived[ 	]*:[ 	]*(true|false)[ 	]*\r?$`)
var agyArchiveFieldName = regexp.MustCompile(`(?m)^[ 	]*archived\b`)

var agyArchiveTimeField = regexp.MustCompile(`(?m)^archival_status_timestamp[ \t]*:?[ \t]*\{\r?\n(?:[ \t]+(?:seconds|nanos)[ \t]*:[ \t]*[0-9]+[ \t]*\r?\n)*\}[ \t]*\r?$`)
var agyArchiveTimeName = regexp.MustCompile(`(?m)^[ \t]*archival_status_timestamp\b`)

func (a agyArchiveAnnotation) withArchive(archived, indexed bool) ([]byte, error) {
	fields := agyArchivedField.FindAllSubmatchIndex(a.before, 2)
	if len(fields) > 1 || len(fields) != len(agyArchiveFieldName.FindAllIndex(a.before, 2)) {
		return nil, errors.New("这个版本的 CLI 会话注解归档字段格式与预期不同")
	}
	out := slices.Clone(a.before)
	flag := strconv.FormatBool(archived)
	if len(fields) == 0 {
		if !archived && !indexed {
			return a.before, nil
		}
		out = appendAgyArchiveField(out, "archived: "+flag)
	} else {
		i := fields[0]
		if string(a.before[i[2]:i[3]]) == flag {
			return a.before, nil
		}
		out = append(append(out[:i[2]], flag...), a.before[i[3]:]...)
	}

	stamps := agyArchiveTimeField.FindAllIndex(out, 2)
	if len(stamps) > 1 || len(stamps) != len(agyArchiveTimeName.FindAllIndex(out, 2)) {
		return nil, errors.New("这个版本的 CLI 归档时间格式与预期不同")
	}
	now := time.Now()
	stamp := fmt.Sprintf("archival_status_timestamp: {\n  seconds: %d\n  nanos: %d\n}", now.Unix(), now.Nanosecond())
	if len(stamps) == 0 {
		return appendAgyArchiveField(out, stamp), nil
	}
	i := stamps[0]
	return append(append(slices.Clone(out[:i[0]]), stamp...), out[i[1]:]...), nil
}

func appendAgyArchiveField(out []byte, field string) []byte {
	if len(out) != 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return append(out, field+"\n"...)
}

func (a agyArchiveAnnotation) restore() error {
	if a.before == nil {
		return os.Remove(a.path)
	}
	return replaceArchiveFile(a.path, a.before, a.mode)
}
