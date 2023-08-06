package parser

import (
	"database/sql"
	"fmt"

	"github.com/pkg/errors"
)

func AllTables(dsn string) ([]string, error) {

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, errors.WithMessage(err, "open db error")
	}
	defer db.Close()
	rows, err := db.Query("SHOW TABLES")

	if err != nil {
		return nil, errors.WithMessage(err, "SHOW TABLES error")
	}
	defer rows.Close()
	// if !rows.Next() {
	// 	return "", errors.Errorf("SHOW TABLES not found")
	// }
	var table string
	var tables []string
	for rows.Next() {
		rows.Scan(&table)
		fmt.Println(table)
		tables = append(tables, table)
	}
	fmt.Println(len(tables))
	// var tables []string
	// var xx []uint8
	// // var createSql string
	// err = rows.Scan(&xx)
	// if err != nil {
	// 	return "", err
	// }
	// log.Println(tables)
	return tables, nil

}
