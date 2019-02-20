package db

type Repository interface {
	Save(data TxInserter) error
}

// SchedulingRepository сохраняет данные с заданной периодичностью period.
// Хранилище накапливает данные за указанный период и после его истечения сохраняет данные в бд в одной транзакции.
// Сохранение происходит в отдельной горутине, поэтому не блокирует вызывающий код.
type SchedulingRepository struct {
	name   string
	period time.Duration

	ch chan TxInserter
}

var _ Repository = (*SchedulingRepository)(nil)

func NewSchedulingRepository(name string, period time.Duration) *SchedulingRepository {
	storage := &SchedulingRepository{
		name:   name,
		period: period,
	}
	ch := make(chan TxInserter)
	storage.ch = ch
	go storage.persistProcessor(ch)
	return storage
}

func (ps *SchedulingRepository) persistProcessor(chData chan TxInserter) {
	var data []TxInserter
	ticker := time.NewTicker(ps.period)
	for {
		select {
		case d := <-chData:
			data = append(data, d)
		case <-ticker.C:
			if len(data) == 0 {
				continue
			}

			go func(data []TxInserter) {
				if err := ExecuteInTx(func(tx *sqlx.Tx) error {
					for _, d := range data {
						if err := d.InsertTx(tx); err != nil {
							return err
						}
					}

					return nil
				}); err != nil {
					log.WithError(err).WithField("persister", ps.name).Errorln("Cannot save in db")
				} else {
					log.WithField("persister", ps.name).Debugln("Successful save in db")
				}
			}(data)

			data = nil
		}
	}
}

func (ps *SchedulingRepository) Save(data TxInserter) error {
	ps.ch <- data
	return nil
}
