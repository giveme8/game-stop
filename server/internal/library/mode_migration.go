package library

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// Run offline before starting writers. MySQL DDL commits implicitly, so each
// step is restartable: add/backfill mode first, then atomically replace the PK.
// Never drop score data or the old key in a separate MySQL statement.
func migrateScoreModes(db *gorm.DB) error {
	for _, table := range []string{"player_scores", "score_submissions"} {
		if !db.Migrator().HasTable(table) {
			continue
		}
		if !db.Migrator().HasColumn(table, "mode") {
			if err := db.Exec("ALTER TABLE " + table + " ADD COLUMN mode varchar(64) NOT NULL DEFAULT 'default'").Error; err != nil {
				return err
			}
		}
		if err := db.Exec("UPDATE " + table + " SET mode = 'default' WHERE mode IS NULL OR mode = ''").Error; err != nil {
			return err
		}
	}
	if !db.Migrator().HasTable("player_scores") {
		return nil
	}
	switch db.Dialector.Name() {
	case "mysql":
		var columns []string
		if err := db.Raw("SELECT COLUMN_NAME FROM information_schema.KEY_COLUMN_USAGE WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'player_scores' AND CONSTRAINT_NAME = 'PRIMARY' ORDER BY ORDINAL_POSITION").Scan(&columns).Error; err != nil {
			return err
		}
		if strings.Join(columns, ",") != "game_id,user_id,board,mode" {
			prefix := ""
			if len(columns) > 0 {
				prefix = "DROP PRIMARY KEY, "
			}
			return db.Exec("ALTER TABLE player_scores " + prefix + "ADD PRIMARY KEY (game_id, user_id, board, mode)").Error
		}
	case "sqlite":
		var columns []struct {
			Name string
			PK   int
		}
		if err := db.Raw("PRAGMA table_info(player_scores)").Scan(&columns).Error; err != nil {
			return err
		}
		for _, c := range columns {
			if c.Name == "mode" && c.PK > 0 {
				return nil
			}
		}
		return db.Transaction(func(tx *gorm.DB) error {
			for _, sql := range []string{
				"CREATE TABLE player_scores_mode_migration (game_id text, user_id integer, board text, mode varchar(64) NOT NULL DEFAULT 'default', value integer NOT NULL, achieved_at datetime NOT NULL, PRIMARY KEY (game_id,user_id,board,mode))",
				"INSERT INTO player_scores_mode_migration SELECT game_id,user_id,board,mode,value,achieved_at FROM player_scores",
				"DROP TABLE player_scores",
				"ALTER TABLE player_scores_mode_migration RENAME TO player_scores",
			} {
				if err := tx.Exec(sql).Error; err != nil {
					return err
				}
			}
			return nil
		})
	default:
		return fmt.Errorf("unsupported score migration database")
	}
	return nil
}
