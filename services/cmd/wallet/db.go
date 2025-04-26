package main

import (
	"database/sql"
	"embed"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"

	"wish/services"

	_ "github.com/lib/pq"
)

const (
	sqlCreateClassTable = `
CREATE TABLE %s
(
    id         UUID      NOT NULL                                                    DEFAULT gen_random_uuid(),
    next       SERIAL    NOT NULL PRIMARY KEY,
    key        VARCHAR   NOT NULL,
    value      VARCHAR   NOT NULL,
    version    INTEGER   NOT NULL                                                    DEFAULT 1,
    status     VARCHAR   NOT NULL CHECK ( status IN ('ITEM_DRAFT', 'ITEM_PUBLISHED', 'ITEM_SKIP') ) DEFAULT 'ITEM_DRAFT',
    before_at  TIMESTAMP                                                             DEFAULT NULL,
    after_at   TIMESTAMP                                                             DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL                                                    DEFAULT CURRENT_TIMESTAMP,
    created_at TIMESTAMP NOT NULL                                                    DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (key, value, version)
)`
	sqlCreateAfterInsertTrigger = `
CREATE TRIGGER %s_after_insert
    AFTER INSERT
    ON %s
    FOR EACH ROW
EXECUTE FUNCTION fn_change_value_after_insert('%s');
`
	sqlCreateChangeStatusTrigger = `
CREATE TRIGGER %s_after_update_status
    AFTER UPDATE
    ON %s
    FOR EACH ROW
    WHEN (NEW.status != OLD.status)
EXECUTE FUNCTION fn_change_value_after_update_status('%s')
`
	sqlCreateAfterUpdateTrigger = `
CREATE TRIGGER %s_after_update_after
    AFTER UPDATE
    ON %s
    FOR EACH ROW
    WHEN (NEW.after_at != OLD.after_at)
EXECUTE FUNCTION fn_change_value_after_update_after('%s')
`
)

//go:embed migrations/*.sql
var migrations embed.FS

type DatabaseClass interface {
	Classes(nameFilter *string, status *string, version *uint32) ([]Class, error)
	Class(name string) (*Class, error)
	CreateClass(name, title string) error
	Elements(c Class, version *uint32, status *string, offset, limit int) ([]Element, int, error)
}

type Class struct {
	Id        uuid.UUID `sql:"id"`
	Name      string    `sql:"name"`
	Title     string    `sql:"title"`
	TableName string    `sql:"table_name"`
	Current   uint32    `sql:"current"`
	Status    string    `sql:"status"`
	UpdatedAt time.Time `sql:"updated_at"`
}
type Element struct {
	Key     string `sql:"key"`
	Value   string `sql:"value"`
	Version uint32 `sql:"version"`
	Status  string `sql:"status"`
}

type ds struct {
	db *sql.DB
}

func (d *ds) CreateClass(name, title string) error {
	tableName := "class_" + name
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	_, err = tx.Exec(
		"INSERT INTO wallet.classes(name, table_name, title) VALUES ($1, $2, $3)", name, tableName, title)
	if err != nil {
		return err
	}
	_, err = tx.Exec(fmt.Sprintf(sqlCreateClassTable, tableName))
	if err != nil {
		return err
	}
	_, err = tx.Exec(fmt.Sprintf(sqlCreateAfterInsertTrigger, tableName, tableName, tableName))
	if err != nil {
		return err
	}
	_, err = tx.Exec(fmt.Sprintf(sqlCreateChangeStatusTrigger, tableName, tableName, tableName))
	if err != nil {
		return err
	}
	_, err = tx.Exec(fmt.Sprintf(sqlCreateAfterUpdateTrigger, tableName, tableName, tableName))
	if err != nil {
		return err
	}
	err = tx.Commit()
	if err != nil {
		return err
	}
	return nil
}

func (d *ds) Elements(c Class, version *uint32, status *string, offset, limit int) ([]Element, int, error) {
	query := fmt.Sprintf("SELECT next, key, value, version, status FROM wallet.\"%s\" WHERE 1 = 1", c.TableName)
	args := make([]interface{}, 0)
	if status != nil {
		query += " AND status  = $" + strconv.Itoa(len(args)+1)
		args = append(args, *status)
	}
	if version != nil {
		query += " AND version = $" + strconv.Itoa(len(args)+1)
		args = append(args, *version)
	}
	query += " ORDER BY next ASC OFFSET $" + strconv.Itoa(len(args)+1) + " LIMIT $" + strconv.Itoa(len(args)+2)
	args = append(args, offset, limit)
	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, offset, err
	}
	defer rows.Close()
	var elements []Element
	var last = offset
	for rows.Next() {
		var element Element
		err = rows.Scan(&last, &element.Key, &element.Value, &element.Version, &element.Status)
		if err != nil {
			return nil, offset, err
		}
		elements = append(elements, element)
	}
	return elements, last, nil
}

func (d *ds) Class(name string) (*Class, error) {
	rows, err := d.db.Query("SELECT id, name, title, table_name, current, status, updated_at  FROM wallet.classes WHERE name = $1", name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var class Class
	if rows.Next() {
		err = rows.Scan(&class.Id, &class.Name, &class.Title, &class.TableName, &class.Current,
			&class.Status, &class.UpdatedAt)
		if err != nil {
			return nil, err
		}
		return &class, nil
	}
	return nil, nil
}

func (d *ds) Classes(nameFilter *string, status *string, version *uint32) ([]Class, error) {
	query := "SELECT id, name, title, table_name, current, status, updated_at FROM wallet.classes WHERE 1 = 1"
	args := make([]interface{}, 0)
	if nameFilter != nil {
		query += " AND name LIKE '%$" + strconv.Itoa(len(args)+1) + "%'"
		args = append(args, *nameFilter)
	}
	if status != nil {
		query += " AND status  = $" + strconv.Itoa(len(args)+1)
		args = append(args, *status)
	}
	if version != nil {
		query += " AND version = $" + strconv.Itoa(len(args)+1)
		args = append(args, *version)
	}

	var rows *sql.Rows
	var err error
	if len(args) == 0 {
		rows, err = d.db.Query(query)
	} else {
		rows, err = d.db.Query(query, args...)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Class, 0)
	for rows.Next() {
		var class Class
		err = rows.Scan(&class.Id, &class.Name, &class.Title, &class.TableName, &class.Current,
			&class.Status, &class.UpdatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, class)
	}
	return result, nil
}

func NewDatabaseClass() DatabaseClass {
	db, err := services.NewDatabase(migrations)
	if err != nil {
		return nil
	}
	return &ds{db}
}
