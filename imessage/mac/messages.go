// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2022 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package mac

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	_ "github.com/mattn/go-sqlite3"

	"go.mau.fi/mautrix-imessage/imessage"
)

const baseMessagesQuery = `
SELECT
  message.ROWID, message.guid, message.date, COALESCE(message.subject, ''), COALESCE(message.text, ''), message.attributedBody,
  chat.guid, COALESCE(sender_handle.id, ''), COALESCE(sender_handle.service, ''), COALESCE(target_handle.id, ''), COALESCE(target_handle.service, ''),
  message.is_from_me, message.date_read, message.is_delivered, message.is_sent, message.is_emote, message.is_audio_message,
  COALESCE(message.thread_originator_guid, ''), COALESCE(message.thread_originator_part, ''), COALESCE(message.associated_message_guid, ''), message.associated_message_type,
  message.group_title, message.item_type, message.group_action_type, chat.group_id
FROM message
JOIN chat_message_join         ON chat_message_join.message_id = message.ROWID
JOIN chat                      ON chat_message_join.chat_id = chat.ROWID
LEFT JOIN handle sender_handle ON message.handle_id = sender_handle.ROWID
LEFT JOIN handle target_handle ON message.other_handle = target_handle.ROWID
`

const attachmentsQuery = `
SELECT guid, filename, COALESCE(mime_type, ''), transfer_name FROM attachment
JOIN message_attachment_join ON message_attachment_join.attachment_id = attachment.ROWID
WHERE message_attachment_join.message_id = $1
ORDER BY ROWID
`

var newMessagesQuery = baseMessagesQuery + `
WHERE message.ROWID > $1
ORDER BY message.date ASC
`

var singleMessageQuery = baseMessagesQuery + `
WHERE message.guid = $1
`

var messagesAfterQuery = baseMessagesQuery + `
WHERE (chat.guid=$1 OR $1='') AND message.date>$2
ORDER BY message.date ASC
`

var messagesBetweenQuery = baseMessagesQuery + `
WHERE (chat.guid=$1 OR $1='') AND message.date>$2 AND message.date<$3
ORDER BY message.date DESC
`

var messagesBeforeWithLimitQuery = baseMessagesQuery + `
WHERE (chat.guid=$1 OR $1='') AND message.date<$2
ORDER BY message.date DESC
LIMIT $3
`

var limitedMessagesQuery = baseMessagesQuery + `
WHERE (chat.guid=$1 OR $1='')
ORDER BY message.date DESC
LIMIT $2
`

const groupActionQuery = `
SELECT attachment.filename, COALESCE(attachment.mime_type, ''), attachment.transfer_name
FROM message
JOIN chat_message_join ON chat_message_join.message_id = message.ROWID
JOIN chat              ON chat_message_join.chat_id = chat.ROWID
LEFT JOIN message_attachment_join ON message_attachment_join.message_id = message.ROWID
LEFT JOIN attachment              ON message_attachment_join.attachment_id = attachment.ROWID
WHERE message.item_type=$1 AND message.group_action_type=$2 AND chat.guid=$3
ORDER BY message.date DESC LIMIT 1
`

const chatQuery = `
SELECT chat_identifier, service_name, COALESCE(display_name, ''), group_id
FROM chat
WHERE guid=$1
`

const chatGUIDQuery = `
SELECT chat.guid FROM chat
JOIN chat_message_join ON chat_message_join.chat_id = chat.ROWID
JOIN message           ON chat_message_join.message_id = message.ROWID
WHERE chat.guid LIKE $1
ORDER BY message.date DESC
LIMIT 1
`

const recentChatsQuery = `
SELECT DISTINCT chat.guid, chat.group_id FROM message
JOIN chat_message_join ON chat_message_join.message_id = message.ROWID
JOIN chat              ON chat_message_join.chat_id = chat.ROWID
WHERE message.date>$1
ORDER BY message.date DESC
`

const newReceiptsQuery = `
SELECT chat.guid, message.guid, message.is_from_me, message.date_read
FROM message
JOIN chat_message_join ON chat_message_join.message_id = message.ROWID
JOIN chat              ON chat_message_join.chat_id = chat.ROWID
WHERE date_read>$1 AND is_read=1
`

func openChatDB() (*sql.DB, string, error) {
	path, err := os.UserHomeDir()
	if err != nil {
		return nil, "", fmt.Errorf("failed to get home directory: %w", err)
	}
	path = filepath.Join(path, "Library", "Messages", "chat.db")
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?mode=ro", path))
	return db, path, err
}

func CheckPermissions() error {
	db, _, err := openChatDB()
	if err != nil {
		return err
	}
	var lastRowIDSQL sql.NullInt32
	err = db.QueryRow("SELECT MAX(ROWID) FROM message").Scan(&lastRowIDSQL)
	if err != nil {
		return err
	} else if !lastRowIDSQL.Valid {
		return imessage.ErrNotLoggedIn
	}
	return nil
}

func (mac *macOSDatabase) prepareMessages() error {
	var err error
	mac.chatDB, mac.chatDBPath, err = openChatDB()
	if err != nil {
		return err
	}
	if !columnExists(mac.chatDB, "message", "thread_originator_guid") {
		messagesAfterQuery = strings.ReplaceAll(messagesAfterQuery, "COALESCE(message.thread_originator_guid, '')", "''")
		messagesBetweenQuery = strings.ReplaceAll(messagesBetweenQuery, "COALESCE(message.thread_originator_guid, '')", "''")
		limitedMessagesQuery = strings.ReplaceAll(limitedMessagesQuery, "COALESCE(message.thread_originator_guid, '')", "''")
		newMessagesQuery = strings.ReplaceAll(newMessagesQuery, "COALESCE(message.thread_originator_guid, '')", "''")
		singleMessageQuery = strings.ReplaceAll(singleMessageQuery, "COALESCE(message.thread_originator_guid, '')", "''")
	}
	if !columnExists(mac.chatDB, "message", "thread_originator_part") {
		messagesAfterQuery = strings.ReplaceAll(messagesAfterQuery, "COALESCE(message.thread_originator_part, '')", "''")
		messagesBetweenQuery = strings.ReplaceAll(messagesBetweenQuery, "COALESCE(message.thread_originator_part, '')", "''")
		limitedMessagesQuery = strings.ReplaceAll(limitedMessagesQuery, "COALESCE(message.thread_originator_part, '')", "''")
		newMessagesQuery = strings.ReplaceAll(newMessagesQuery, "COALESCE(message.thread_originator_part, '')", "''")
		singleMessageQuery = strings.ReplaceAll(singleMessageQuery, "COALESCE(message.thread_originator_part, '')", "''")
	}
	if !columnExists(mac.chatDB, "message", "group_action_type") {
		messagesAfterQuery = strings.ReplaceAll(messagesAfterQuery, "message.group_action_type", "0")
		messagesBetweenQuery = strings.ReplaceAll(messagesBetweenQuery, "message.group_action_type", "0")
		limitedMessagesQuery = strings.ReplaceAll(limitedMessagesQuery, "message.group_action_type", "0")
		newMessagesQuery = strings.ReplaceAll(newMessagesQuery, "message.group_action_type", "0")
		singleMessageQuery = strings.ReplaceAll(singleMessageQuery, "message.group_action_type", "0")
	}
	mac.messagesAfterQuery, err = mac.chatDB.Prepare(messagesAfterQuery)
	if err != nil {
		return fmt.Errorf("failed to prepare message query: %w", err)
	}
	mac.messagesBetweenQuery, err = mac.chatDB.Prepare(messagesBetweenQuery)
	if err != nil {
		return fmt.Errorf("failed to prepare message query: %w", err)
	}
	mac.messagesBeforeWithLimitQuery, err = mac.chatDB.Prepare(messagesBeforeWithLimitQuery)
	if err != nil {
		return fmt.Errorf("failed to prepare message query: %w", err)
	}
	mac.singleMessageQuery, err = mac.chatDB.Prepare(singleMessageQuery)
	if err != nil {
		return fmt.Errorf("failed to prepare single message query: %w", err)
	}
	mac.attachmentsQuery, err = mac.chatDB.Prepare(attachmentsQuery)
	if err != nil {
		return fmt.Errorf("failed to prepare attachments query: %w", err)
	}
	mac.groupActionQuery, err = mac.chatDB.Prepare(groupActionQuery)
	if err != nil {
		mac.log.Warnln("Failed to prepare group action query:", err)
		mac.groupActionQuery = nil
	}
	mac.limitedMessagesQuery, err = mac.chatDB.Prepare(limitedMessagesQuery)
	if err != nil {
		return fmt.Errorf("failed to prepare limited message query: %w", err)
	}
	mac.newMessagesQuery, err = mac.chatDB.Prepare(newMessagesQuery)
	if err != nil {
		return fmt.Errorf("failed to prepare new message query: %w", err)
	}
	mac.newReceiptsQuery, err = mac.chatDB.Prepare(newReceiptsQuery)
	if err != nil {
		return fmt.Errorf("failed to prepare new receipt query: %w", err)
	}
	mac.chatQuery, err = mac.chatDB.Prepare(chatQuery)
	if err != nil {
		return fmt.Errorf("failed to prepare chat query: %w", err)
	}
	mac.chatGUIDQuery, err = mac.chatDB.Prepare(chatGUIDQuery)
	if err != nil {
		return fmt.Errorf("failed to prepare chat GUID query: %w", err)
	}
	mac.recentChatsQuery, err = mac.chatDB.Prepare(recentChatsQuery)
	if err != nil {
		return fmt.Errorf("failed to prepare recent chats query: %w", err)
	}

	mac.Messages = make(chan *imessage.Message)
	mac.ReadReceipts = make(chan *imessage.ReadReceipt)
	return nil
}

func (mac *macOSDatabase) scanMessages(res *sql.Rows) (messages []*imessage.Message, err error) {
	for res.Next() {
		var message imessage.Message
		var tapback imessage.Tapback
		var attributedBody []byte
		var timestamp int64
		var readAt int64
		var newGroupTitle sql.NullString
		var threadOriginatorPart string
		err = res.Scan(&message.RowID, &message.GUID, &timestamp, &message.Subject, &message.Text, &attributedBody,
			&message.ChatGUID, &message.Sender.LocalID, &message.Sender.Service, &message.Target.LocalID, &message.Target.Service,
			&message.IsFromMe, &readAt, &message.IsDelivered, &message.IsSent, &message.IsEmote, &message.IsAudioMessage,
			&message.ReplyToGUID, &threadOriginatorPart, &tapback.TargetGUID, &tapback.Type,
			&newGroupTitle, &message.ItemType, &message.GroupActionType, &message.ThreadID)
		if err != nil {
			err = fmt.Errorf("error scanning row: %w", err)
			return
		}
		message.Time = time.Unix(imessage.AppleEpoch.Unix(), timestamp)
		if readAt != 0 {
			message.ReadAt = time.Unix(imessage.AppleEpoch.Unix(), readAt)
			message.IsRead = true
		}
		message.Attachments = make([]*imessage.Attachment, 0)
		var ares *sql.Rows
		ares, err = mac.attachmentsQuery.Query(message.RowID)
		if err != nil {
			err = fmt.Errorf("error querying attachments for %d: %w", message.RowID, err)
			return
		}
		for ares.Next() {
			var attachment imessage.Attachment
			err = ares.Scan(&attachment.GUID, &attachment.PathOnDisk, &attachment.MimeType, &attachment.FileName)
			if err != nil {
				err = fmt.Errorf("error scanning attachment row for %d: %w", message.RowID, err)
				return
			}
			message.Attachments = append(message.Attachments, &attachment)
		}
		if len(attributedBody) > 0 {
			//fmt.Println(base64.StdEncoding.EncodeToString(attributedBody))
			var decoded *AttributedString
			decoded, err = meowDecodeAttributedString(attributedBody)
			if err != nil {
				mac.log.Warnfln("Failed to decode attributedBody of %s: %v", message.GUID, err)
			} else {
				//d, _ := json.MarshalIndent(decoded, "", "  ")
				//fmt.Println(string(d))
				if len(message.Text) == 0 && len(decoded.Content) > 0 {
					message.Text = strings.TrimSpace(decoded.Content)
				}
				message.Attachments = decoded.SortAttachments(mac.log, message.Attachments)
			}
		}
		if len(message.Attachments) > 0 {
			message.Attachment = message.Attachments[0]
		}

		if newGroupTitle.Valid {
			message.NewGroupName = newGroupTitle.String
		}
		if len(threadOriginatorPart) > 0 {
			// The thread_originator_part field seems to have three parts separated by colons.
			// The first two parts look like the part index, the third one is something else.
			// TODO this might not be reliable
			message.ReplyToPart, _ = strconv.Atoi(strings.Split(threadOriginatorPart, ":")[0])
		}
		if message.IsFromMe {
			message.Sender.LocalID = ""
		}
		if len(tapback.TargetGUID) > 0 {
			message.Tapback, err = tapback.Parse()
			if err != nil {
				mac.log.Warnfln("Failed to parse tapback in %s: %v", message.GUID, err)
			}
		}
		messages = append(messages, &message)
	}
	return
}

func reverseArray(messages []*imessage.Message) {
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
}

func columnExists(db *sql.DB, table, column string) bool {
	row := db.QueryRow(fmt.Sprintf(`SELECT name FROM pragma_table_info("%s") WHERE name=$1;`, table), column)
	var name string
	_ = row.Scan(&name)
	return name == column
}

func (mac *macOSDatabase) GetMessagesWithLimit(chatID string, limit int, backfillID string) ([]*imessage.Message, error) {
	res, err := mac.limitedMessagesQuery.Query(chatID, limit)
	if err != nil {
		return nil, fmt.Errorf("error querying messages with limit: %w", err)
	}
	messages, err := mac.scanMessages(res)
	if err != nil {
		return messages, err
	}
	reverseArray(messages)
	return messages, err
}

func (mac *macOSDatabase) GetMessagesSinceDate(chatID string, minDate time.Time, _ string) ([]*imessage.Message, error) {
	res, err := mac.messagesAfterQuery.Query(chatID, minDate.UnixNano()-imessage.AppleEpoch.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("error querying messages after date: %w", err)
	}
	return mac.scanMessages(res)
}

func (mac *macOSDatabase) GetMessagesBetween(chatID string, minDate time.Time, maxDate time.Time) ([]*imessage.Message, error) {
	res, err := mac.messagesBetweenQuery.Query(chatID,
		minDate.UnixNano()-imessage.AppleEpoch.UnixNano(),
		maxDate.UnixNano()-imessage.AppleEpoch.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("error querying messages between dates: %w", err)
	}
	return mac.scanMessages(res)
}

func (mac *macOSDatabase) GetMessagesBeforeWithLimit(chatID string, before time.Time, limit int) ([]*imessage.Message, error) {
	res, err := mac.messagesBeforeWithLimitQuery.Query(chatID, before.UnixNano()-imessage.AppleEpoch.UnixNano(), limit)
	if err != nil {
		return nil, fmt.Errorf("error querying messages before date with limit: %w", err)
	}
	return mac.scanMessages(res)
}

func (mac *macOSDatabase) GetMessage(guid string) (*imessage.Message, error) {
	res, err := mac.singleMessageQuery.Query(guid)
	if err != nil {
		return nil, fmt.Errorf("error querying single message: %w", err)
	}
	msgs, err := mac.scanMessages(res)
	if err != nil {
		return nil, err
	}
	if len(msgs) > 0 {
		return msgs[1], nil
	}
	return nil, nil
}

func (mac *macOSDatabase) getMessagesSinceRowID(rowID int) ([]*imessage.Message, error) {
	res, err := mac.newMessagesQuery.Query(rowID)
	if err != nil {
		return nil, fmt.Errorf("error querying messages after rowid: %w", err)
	}
	return mac.scanMessages(res)
}

func (mac *macOSDatabase) getReadReceiptsSince(minDate time.Time) ([]*imessage.ReadReceipt, time.Time, error) {
	origMinDate := minDate.UnixNano() - imessage.AppleEpoch.UnixNano()
	res, err := mac.newReceiptsQuery.Query(origMinDate)
	if err != nil {
		return nil, minDate, fmt.Errorf("error querying read receipts after date: %w", err)
	}
	var receipts []*imessage.ReadReceipt
	for res.Next() {
		var chatGUID, messageGUID string
		var messageIsFromMe bool
		var readAtAppleEpoch int64
		err = res.Scan(&chatGUID, &messageGUID, &messageIsFromMe, &readAtAppleEpoch)
		if err != nil {
			return receipts, minDate, fmt.Errorf("error scanning row: %w", err)
		}
		readAt := time.Unix(imessage.AppleEpoch.Unix(), readAtAppleEpoch)
		if readAtAppleEpoch > origMinDate {
			minDate = readAt
		}

		receipt := &imessage.ReadReceipt{
			ChatGUID: chatGUID,
			ReadUpTo: messageGUID,
			ReadAt:   readAt,
		}
		if messageIsFromMe {
			// For messages from me, the receipt is not from me, and vice versa.
			receipt.IsFromMe = false
			if imessage.ParseIdentifier(chatGUID).IsGroup {
				// We don't get read receipts from other users in groups,
				// so skip our own messages.
				continue
			} else {
				// The read receipt is on our own message and it's a private chat,
				// which means the read receipt is from the private chat recipient.
				receipt.SenderGUID = chatGUID
			}
		} else {
			receipt.IsFromMe = true
		}
		receipts = append(receipts, receipt)
	}
	return receipts, minDate, nil
}

func (mac *macOSDatabase) GetChatsWithMessagesAfter(minDate time.Time) ([]imessage.ChatIdentifier, error) {
	res, err := mac.recentChatsQuery.Query(minDate.UnixNano() - imessage.AppleEpoch.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("error querying chats with messages after date: %w", err)
	}
	var chats []imessage.ChatIdentifier
	for res.Next() {
		var chatID, groupID string
		err = res.Scan(&chatID, &groupID)
		if err != nil {
			return chats, fmt.Errorf("error scanning row: %w", err)
		}
		chats = append(chats, imessage.ChatIdentifier{ChatGUID: chatID, ThreadID: groupID})
	}
	return chats, nil
}

func (mac *macOSDatabase) GetChatInfo(chatID, _ string) (*imessage.ChatInfo, error) {
	row := mac.chatQuery.QueryRow(chatID)
	var info imessage.ChatInfo
	info.Identifier = imessage.ParseIdentifier(chatID)
	err := row.Scan(&info.Identifier.LocalID, &info.Identifier.Service, &info.DisplayName, &info.ThreadID)
	if err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return &info, err
	}
	info.Members, err = mac.GetGroupMembers(chatID)
	return &info, err
}

func (mac *macOSDatabase) ResolveIdentifier(identifier string) (guid string, err error) {
	err = mac.chatGUIDQuery.QueryRow("%;-;" + identifier).Scan(&guid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("user not found")
	}
	return
}

func (mac *macOSDatabase) GetGroupAvatar(chatID string) (*imessage.Attachment, error) {
	if mac.groupActionQuery == nil {
		return nil, nil
	}
	row := mac.groupActionQuery.QueryRow(imessage.ItemTypeAvatar, imessage.GroupActionSetAvatar, chatID)
	var avatar imessage.Attachment
	err := row.Scan(&avatar.PathOnDisk, &avatar.MimeType, &avatar.FileName)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &avatar, err
}

func (mac *macOSDatabase) Stop() {
	mac.stopWatching <- struct{}{}
	mac.stopWakeupDetecting <- struct{}{}
	mac.stopWait.Wait()
}

func (mac *macOSDatabase) MessageChan() <-chan *imessage.Message {
	return mac.Messages
}

func (mac *macOSDatabase) ReadReceiptChan() <-chan *imessage.ReadReceipt {
	return mac.ReadReceipts
}

func (mac *macOSDatabase) TypingNotificationChan() <-chan *imessage.TypingNotification {
	return nil
}

func (mac *macOSDatabase) ChatChan() <-chan *imessage.ChatInfo {
	return nil
}

func (mac *macOSDatabase) ContactChan() <-chan *imessage.Contact {
	return nil
}

func (mac *macOSDatabase) MessageStatusChan() <-chan *imessage.SendMessageStatus {
	return nil
}

func (mac *macOSDatabase) BackfillTaskChan() <-chan *imessage.BackfillTask {
	return nil
}

func (mac *macOSDatabase) Start(readyCallback func()) error {
	mac.stopWait.Add(2)
	go mac.ListenWakeup()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create fsnotify watcher: %w", err)
	}
	defer watcher.Close()

	stop := make(chan struct{}, 1)
	mac.stopWatching = stop

	err = watcher.Add(filepath.Dir(mac.chatDBPath))
	if err != nil {
		return fmt.Errorf("failed to add chat DB to fsnotify watcher: %w", err)
	}

	var dropEvents bool
	var handleLock sync.Mutex
	nonSentMessages := make(map[string]bool)
	minReceiptTime := time.Now()
	var lastRowIDSQL sql.NullInt32
	err = mac.chatDB.QueryRow("SELECT MAX(ROWID) FROM message").Scan(&lastRowIDSQL)
	if err != nil {
		return fmt.Errorf("failed to fetch last row ID: %w", err)
	} else if !lastRowIDSQL.Valid {
		return imessage.ErrNotLoggedIn
	}
	readyCallback()
	lastRowID := int(lastRowIDSQL.Int32)
Loop:
	for {
		select {
		case _, ok := <-watcher.Events:
			if !ok {
				break Loop
			} else if dropEvents {
				continue
			}
			dropEvents = true
			go func() {
				handleLock.Lock()
				defer handleLock.Unlock()
				time.Sleep(50 * time.Millisecond)
				dropEvents = false
				newMessages, err := mac.getMessagesSinceRowID(lastRowID)
				if err != nil {
					mac.log.Warnln("Error reading messages after fsevent:", err)
				}
				for _, message := range newMessages {
					if message.RowID > lastRowID {
						lastRowID = message.RowID
					}

					if !message.IsSent {
						nonSentMessages[message.GUID] = true
					} else if _, ok := nonSentMessages[message.GUID]; ok {
						delete(nonSentMessages, message.GUID)
						continue
					}

					mac.Messages <- message
				}
				var newReceipts []*imessage.ReadReceipt
				newReceipts, minReceiptTime, err = mac.getReadReceiptsSince(minReceiptTime)
				if err != nil {
					mac.log.Warnln("Error reading receipts after fsevent:", err)
				}
				for _, receipt := range newReceipts {
					mac.ReadReceipts <- receipt
				}
			}()
		case err := <-watcher.Errors:
			return fmt.Errorf("error in watcher: %w", err)
		case <-stop:
			break Loop
		}
	}
	mac.stopWait.Done()
	return nil
}
