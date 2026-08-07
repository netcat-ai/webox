// Package ilink defines the public Go representation of the official iLink
// message protocol plus Webox's local shared_path media extension.
package ilink

const (
	MessageTypeNone = iota
	MessageTypeUser
	MessageTypeBot
)

const (
	ItemTypeNone = iota
	ItemTypeText
	ItemTypeImage
	ItemTypeVoice
	ItemTypeFile
	ItemTypeVideo
)

const (
	MessageStateNew = iota
	MessageStateGenerating
	MessageStateFinish
)

type CDNMedia struct {
	EncryptQueryParam string `json:"encrypt_query_param,omitempty"`
	AESKey            string `json:"aes_key,omitempty"`
	EncryptType       int    `json:"encrypt_type,omitempty"`
	FullURL           string `json:"full_url,omitempty"`
}

type TextItem struct {
	Text string `json:"text,omitempty"`
}

type ImageItem struct {
	Media       *CDNMedia `json:"media,omitempty"`
	ThumbMedia  *CDNMedia `json:"thumb_media,omitempty"`
	AESKey      string    `json:"aeskey,omitempty"`
	URL         string    `json:"url,omitempty"`
	MidSize     int64     `json:"mid_size,omitempty"`
	ThumbSize   int64     `json:"thumb_size,omitempty"`
	ThumbHeight int64     `json:"thumb_height,omitempty"`
	ThumbWidth  int64     `json:"thumb_width,omitempty"`
	HDSize      int64     `json:"hd_size,omitempty"`
	SharedPath  string    `json:"shared_path,omitempty"` // Webox local-media extension.
}

type VoiceItem struct {
	Media         *CDNMedia `json:"media,omitempty"`
	EncodeType    int       `json:"encode_type,omitempty"`
	BitsPerSample int       `json:"bits_per_sample,omitempty"`
	SampleRate    int       `json:"sample_rate,omitempty"`
	Playtime      int       `json:"playtime,omitempty"`
	Text          string    `json:"text,omitempty"`
}

type FileItem struct {
	Media      *CDNMedia `json:"media,omitempty"`
	FileName   string    `json:"file_name,omitempty"`
	MD5        string    `json:"md5,omitempty"`
	Length     string    `json:"len,omitempty"`
	SharedPath string    `json:"shared_path,omitempty"` // Webox local-media extension.
}

type VideoItem struct {
	Media       *CDNMedia `json:"media,omitempty"`
	VideoSize   int64     `json:"video_size,omitempty"`
	PlayLength  int       `json:"play_length,omitempty"`
	VideoMD5    string    `json:"video_md5,omitempty"`
	ThumbMedia  *CDNMedia `json:"thumb_media,omitempty"`
	ThumbSize   int64     `json:"thumb_size,omitempty"`
	ThumbHeight int64     `json:"thumb_height,omitempty"`
	ThumbWidth  int64     `json:"thumb_width,omitempty"`
}

type ToolCallStartItem struct {
	ToolName   string `json:"tool_name,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type ToolCallResultItem struct {
	ToolName   string `json:"tool_name,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Status     string `json:"status,omitempty"`
}

type RefMessage struct {
	MessageItem *MessageItem `json:"message_item,omitempty"`
	Title       string       `json:"title,omitempty"`
}

type MessageItem struct {
	Type           int                 `json:"type,omitempty"`
	CreateTimeMS   int64               `json:"create_time_ms,omitempty"`
	UpdateTimeMS   int64               `json:"update_time_ms,omitempty"`
	IsCompleted    bool                `json:"is_completed,omitempty"`
	MessageID      string              `json:"msg_id,omitempty"`
	Reference      *RefMessage         `json:"ref_msg,omitempty"`
	Text           *TextItem           `json:"text_item,omitempty"`
	Image          *ImageItem          `json:"image_item,omitempty"`
	Voice          *VoiceItem          `json:"voice_item,omitempty"`
	File           *FileItem           `json:"file_item,omitempty"`
	Video          *VideoItem          `json:"video_item,omitempty"`
	ToolCallStart  *ToolCallStartItem  `json:"tool_call_start_item,omitempty"`
	ToolCallResult *ToolCallResultItem `json:"tool_call_result_item,omitempty"`
}

type WeixinMessage struct {
	Sequence     int64         `json:"seq,omitempty"`
	MessageID    int64         `json:"message_id,omitempty"`
	FromUserID   string        `json:"from_user_id,omitempty"`
	ToUserID     string        `json:"to_user_id,omitempty"`
	ClientID     string        `json:"client_id,omitempty"`
	CreateTimeMS int64         `json:"create_time_ms,omitempty"`
	UpdateTimeMS int64         `json:"update_time_ms,omitempty"`
	DeleteTimeMS int64         `json:"delete_time_ms,omitempty"`
	SessionID    string        `json:"session_id,omitempty"`
	GroupID      string        `json:"group_id,omitempty"`
	MessageType  int           `json:"message_type,omitempty"`
	MessageState int           `json:"message_state,omitempty"`
	Items        []MessageItem `json:"item_list,omitempty"`
	ContextToken string        `json:"context_token,omitempty"`
	RunID        string        `json:"run_id,omitempty"`
	// TODO: replace this temporary extension when group mention handling is redesigned.
	MentionedMe bool `json:"mentioned_me,omitempty"`

	MentionedUserIDs []string `json:"-"`
}
