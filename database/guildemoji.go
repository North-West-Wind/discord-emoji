package database

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/bwmarrin/discordgo"
	"go.mau.fi/util/dbutil"
	log "maunium.net/go/maulogger/v2"
)

type GuildEmojiQuery struct {
	db  *Database
	log log.Logger
}

const (
	guildReactionSelect = "SELECT dc_guild_id, dc_emoji_name, mxc, animated FROM guild_emoji"
)

func (geq *GuildEmojiQuery) New() *GuildEmoji {
	return &GuildEmoji{
		db:  geq.db,
		log: geq.log,
	}
}

func (geq *GuildEmojiQuery) GetAllByGuildID(guildID string) []*GuildEmoji {
	query := guildReactionSelect + " WHERE dc_guild_id=$1"

	return geq.getAll(query, guildID)
}

func (geq *GuildEmojiQuery) getAll(query string, args ...interface{}) []*GuildEmoji {
	rows, err := geq.db.Query(query, args...)
	if err != nil || rows == nil {
		return nil
	}

	var guildEmojis []*GuildEmoji
	for rows.Next() {
		guildEmojis = append(guildEmojis, geq.New().Scan(rows))
	}

	return guildEmojis
}

func (geq *GuildEmojiQuery) GetByMXC(mxc string) *GuildEmoji {
	query := guildReactionSelect + " WHERE mxc=$1"

	return geq.get(query, mxc)
}

func (geq *GuildEmojiQuery) GetByAlt(alt string) *GuildEmoji {
	query := guildReactionSelect + " WHERE dc_emoji_name_='$1:%'"

	return geq.get(query, alt)
}

func (geq *GuildEmojiQuery) get(query string, args ...interface{}) *GuildEmoji {
	row := geq.db.QueryRow(query, args...)
	if row == nil {
		return nil
	}

	return geq.New().Scan(row)
}

type GuildEmoji struct {
	db  *Database
	log log.Logger

	GuildID   string
	EmojiName string
	MXC       string
	Animated  bool
}

func (e *GuildEmoji) Scan(row dbutil.Scannable) *GuildEmoji {
	err := row.Scan(&e.GuildID, &e.EmojiName, &e.MXC, &e.Animated)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			e.log.Errorln("Database scan failed:", err)
			panic(err)
		}
		return nil
	}

	return e
}

func (e *GuildEmoji) Insert() {
	query := `
		INSERT INTO guild_emoji (dc_guild_id, dc_emoji_name, mxc, animated)
		VALUES($1, $2, $3, $4)
	`
	_, err := e.db.Exec(query, e.GuildID, e.EmojiName, e.MXC, e.Animated)
	if err != nil {
		e.log.Warnfln("Failed to insert reaction for %s@%s: %v", e.GuildID, e.EmojiName, err)
		panic(err)
	}
}

func (e *GuildEmoji) Delete() {
	query := "DELETE FROM guild_emoji WHERE dc_guild_id=$1 AND dc_emoji_name=$2"
	_, err := e.db.Exec(query, e.GuildID, e.EmojiName)
	if err != nil {
		e.log.Warnfln("Failed to delete reaction for %s@%s: %v", e.GuildID, e.EmojiName, err)
		panic(err)
	}
}

func (e *GuildEmoji) FromDiscord(guildID string, emoji *discordgo.Emoji) {
	e.GuildID = guildID
	e.EmojiName = fmt.Sprintf("%s:%s", emoji.Name, emoji.ID)
	e.Animated = emoji.Animated
}
