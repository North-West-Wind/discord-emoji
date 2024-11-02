package database

import (
	_ "embed"

	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/maulogger/v2"

	"go.mau.fi/mautrix-discord/database/upgrades"
)

type Database struct {
	*dbutil.Database

	User     *UserQuery
	Portal   *PortalQuery
	Puppet   *PuppetQuery
	Message  *MessageQuery
	Thread   *ThreadQuery
	Reaction *ReactionQuery
	Guild    *GuildQuery
	Role     *RoleQuery
	File     *FileQuery
	Emoticon *EmoticonQuery
}

func New(baseDB *dbutil.Database, log maulogger.Logger) *Database {
	db := &Database{Database: baseDB}
	db.UpgradeTable = upgrades.Table
	db.User = &UserQuery{
		db:  db,
		log: log.Sub("User"),
	}
	db.Portal = &PortalQuery{
		db:  db,
		log: log.Sub("Portal"),
	}
	db.Puppet = &PuppetQuery{
		db:  db,
		log: log.Sub("Puppet"),
	}
	db.Message = &MessageQuery{
		db:  db,
		log: log.Sub("Message"),
	}
	db.Thread = &ThreadQuery{
		db:  db,
		log: log.Sub("Thread"),
	}
	db.Reaction = &ReactionQuery{
		db:  db,
		log: log.Sub("Reaction"),
	}
	db.Guild = &GuildQuery{
		db:  db,
		log: log.Sub("Guild"),
	}
	db.Role = &RoleQuery{
		db:  db,
		log: log.Sub("Role"),
	}
	db.File = &FileQuery{
		db:  db,
		log: log.Sub("File"),
	}
	db.Emoticon = &EmoticonQuery{
		db:  db,
		log: log.Sub("Emoticon"),
	}
	return db
}

func strPtr[T ~string](val T) *string {
	if val == "" {
		return nil
	}
	valStr := string(val)
	return &valStr
}
