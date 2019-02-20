package db

type DriverDB string

func DriverFromString(driverName string) (DriverDB, error) {
	switch driverName {
	case "oracle":
		return Oracle, nil
	case "postgres":
		return PostgreSQL, nil
	default:
		return "", fmt.Errorf("unrecognized driver: %s", driverName)
	}
}

const (
	PostgreSQL DriverDB = "postgres"
	Oracle     DriverDB = "oci8"
)

type TxInserter interface {
	InsertTx(*sqlx.Tx) error
}

var db *sqlx.DB

func InitDB(driver DriverDB, dataSource string) (err error) {
	db, err = sqlx.Connect(string(driver), dataSource)
	return
}

func SetMaxConns(maxConnst int) {
	db.SetMaxOpenConns(maxConnst)
}

func ExecuteInTx(f func(tx *sqlx.Tx) error) (err error) {
	tx, err := db.Beginx()
	if err != nil {
		return err
	}

	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			panic(p)
		} else if err != nil {
			tx.Rollback()
		} else {
			err = tx.Commit()
		}
	}()

	err = f(tx)
	return
}

func DeleteOutdatedData() error {
	return ExecuteInTx(func(tx *sqlx.Tx) error {
		threshold := time.Now().AddDate(0, 0, -1)

		for _, query := range [...]string{
			"delete from ns_acw acw where acw.exit_time < ?",
			"delete from ns_agent_status_duration asd where asd.collected_ts < ?",
			"delete from ns_agent_sub_status_duration assd where assd.collected_ts < ?",
			"delete from ns_blocked_calls bc where bc.collected_ts < ?",
			"delete from ns_inbound_call_data ic where ic.created_ts < ?",
			"delete from ns_inbound_project_summary ip where ip.collected_ts < ?",
			"delete from ns_leg_data ld where ld.created_ts < ?",
			"delete from ns_outbound_call_data oc where oc.created_ts < ?",
			"delete from ns_outbound_project_summary op where op.collected_ts < ?",
		} {
			if _, err := tx.Exec(db.Rebind(query), threshold); err != nil {
				return err
			}
		}

		return nil
	})
}
