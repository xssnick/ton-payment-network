package filedb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xssnick/payment-network/internal/node/db"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (d *FileDB) CreateTask(typ, id string, data any, executeAfter, executeTill *time.Time) error {
	d.mx.Lock()
	defer d.mx.Unlock()

	bts, err := json.Marshal(data)
	if err != nil {
		return err
	}

	tg, err := d.getTask(id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			// TODO: fast track to execute via channel

			return d.createTask(&db.Task{
				ID:           id,
				Type:         typ,
				Data:         bts,
				ExecuteAfter: executeAfter,
				ExecuteTill:  executeTill,
				CreatedAt:    time.Now(),
			})
		}
		return err
	}

	if !bytes.Equal(tg.Data, bts) {
		return fmt.Errorf("task with this id and another data is already exists")
	}
	// already exists
	return nil
}

func (d *FileDB) createTask(task *db.Task) error {
	_ = os.MkdirAll(d.dir+"/tasks/", os.ModePerm)

	path := d.dir + "/tasks/" + task.ID + ".json"

	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		fl, err := os.Create(path)
		if err != nil {
			return fmt.Errorf("failed to create file: %w", err)
		}
		defer fl.Close()

		if err = json.NewEncoder(fl).Encode(task); err != nil {
			return fmt.Errorf("failed to encode json to file: %w", err)
		}
		return nil
	}

	if err == nil {
		return db.ErrAlreadyExists
	}
	return err
}

func (d *FileDB) updateTask(t *db.Task) error {
	path := d.dir + "/tasks/" + t.ID + ".json"

	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("failed to stat file: %w", err)
	}

	fl, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer fl.Close()

	if err = json.NewEncoder(fl).Encode(t); err != nil {
		return fmt.Errorf("failed to write json file: %w", err)
	}
	return nil
}

func (d *FileDB) RetryTask(id string, reason string, retryAt time.Time) error {
	d.mx.Lock()
	defer d.mx.Unlock()

	t, err := d.getTask(id)
	if err != nil {
		return err
	}

	if t.IsExecuted {
		return nil
	}
	t.LastError = reason
	t.ExecuteAfter = &retryAt

	return d.updateTask(t)
}

func (d *FileDB) CompleteTask(id string) error {
	d.mx.Lock()
	defer d.mx.Unlock()

	t, err := d.getTask(id)
	if err != nil {
		return err
	}

	if t.IsExecuted {
		return nil
	}
	t.IsExecuted = true

	return d.updateTask(t)
}

func (d *FileDB) getTask(id string) (*db.Task, error) {
	path := d.dir + "/tasks/" + id + ".json"

	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, db.ErrNotFound
	}

	fl, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer fl.Close()

	var task *db.Task
	if err = json.NewDecoder(fl).Decode(&task); err != nil {
		return nil, fmt.Errorf("failed to decode json file: %w", err)
	}
	return task, nil
}

func (d *FileDB) AcquireTask() (*db.Task, error) {
	d.mx.Lock()
	defer d.mx.Unlock()

	var task *db.Task
	err := filepath.Walk(d.dir+"/tasks/", func(path string, info fs.FileInfo, err error) error {
		if task != nil {
			return nil
		}

		if strings.HasSuffix(path, ".json") {
			spl := strings.Split(path, "/")
			name := spl[len(spl)-1]

			t, err := d.getTask(name[:len(name)-len(".json")])
			if err != nil {
				return err
			}

			if !t.IsExecuted && (t.ExecuteAfter == nil || t.ExecuteAfter.Before(time.Now())) &&
				(t.ExecuteTill == nil || t.ExecuteTill.After(time.Now())) {
				task = t
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return task, nil
}
