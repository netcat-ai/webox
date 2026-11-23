package wechatapi

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/netcat-ai/webox/internal/wechatdb"
	"github.com/netcat-ai/webox/wecom"
)

var errInboundMediaNotReady = errors.New("inbound media is not ready")

func (server *Server) getMedia(response http.ResponseWriter, request *http.Request) {
	if !server.authenticate(response, request) {
		return
	}
	query := request.URL.Query()
	roomID, fileID, accountID := query.Get("roomid"), query.Get("sdkfileid"), query.Get("account_id")
	if roomID == "" || fileID == "" || accountID == "" {
		writeError(response, http.StatusBadRequest, "roomid, sdkfileid and account_id are required")
		return
	}
	if !server.session.IsInitialized() {
		writeJSON(response, http.StatusConflict, sessionUnavailable())
		return
	}
	account, err := server.session.UserInfo()
	if err != nil {
		writeJSON(response, http.StatusConflict, sessionUnavailable())
		return
	}
	if account.AccountID != accountID {
		writeError(response, http.StatusConflict, "WeChat account changed")
		return
	}
	contacts, err := server.contacts.ContactsByRoomID(roomID)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "could not query WeChat contacts")
		return
	}
	for _, contact := range contacts {
		if contact.RoomID == roomID && strings.HasPrefix(strings.ToLower(contact.Remark), "webox.") && server.media != nil {
			server.writeMedia(response, roomID, fileID)
			return
		}
	}
	writeError(response, http.StatusNotFound, "media unavailable")
}

func (server *Server) writeMedia(response http.ResponseWriter, roomID, fileID string) {
	file, err := server.media.OpenInbox(roomID, fileID)
	if err != nil {
		writeError(response, http.StatusNotFound, "media unavailable")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		writeError(response, http.StatusNotFound, "media unavailable")
		return
	}
	response.Header().Set("Content-Type", "application/octet-stream")
	response.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	response.Header().Set("Cache-Control", "no-store")
	_, _ = io.CopyN(response, file, info.Size())
}

func inboundMediaWaitExpired(message wecom.Message, now time.Time) bool {
	return message.MsgTime <= 0 || !now.Before(time.UnixMilli(message.MsgTime).Add(inboundMediaWait))
}

func (server *Server) materializeInboundImage(roomID string, message wecom.Message) (string, error) {
	if server.media == nil {
		return "", errors.New("shared media store is unavailable")
	}
	media, err := server.rooms.ReadImage(roomID, message.Sequence, time.UnixMilli(message.MsgTime).Unix(), message.Image.MD5Sum)
	if errors.Is(err, wechatdb.ErrImageDecode) {
		return "", fmt.Errorf("%w: %w", errInboundMediaNotReady, err)
	}
	if err != nil {
		return "", fmt.Errorf("read WeChat image: %w", err)
	}
	if media == nil {
		return "", errInboundMediaNotReady
	}
	sharedPath, err := server.media.WriteInbox(roomID, message.MsgID, media.Data, media.ContentType)
	if err != nil {
		return "", fmt.Errorf("write WeChat image to shared directory: %w", err)
	}
	return sharedPath, nil
}

func (server *Server) materializeInboundFile(roomID string, message wecom.Message) (*wechatdb.LocalFile, string, error) {
	if server.media == nil {
		return nil, "", errors.New("shared media store is unavailable")
	}
	file, err := server.rooms.ReadFile(roomID, message.Sequence, time.UnixMilli(message.MsgTime).Unix(), message.File.FileName)
	if err != nil {
		return nil, "", fmt.Errorf("read WeChat file: %w", err)
	}
	if file == nil {
		return nil, "", errInboundMediaNotReady
	}
	sharedPath, err := server.media.CopyInboxFile(roomID, message.MsgID, file.Filename, file.Path)
	if err != nil {
		return nil, "", fmt.Errorf("write WeChat file to shared directory: %w", err)
	}
	return file, sharedPath, nil
}
