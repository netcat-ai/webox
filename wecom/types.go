// Package wecom defines the message JSON format used by the WeCom
// conversation archive API. Webox uses this format for both inbound and
// outbound WeChat messages.
package wecom

const ActionSend = "send"

const (
	MessageTypeText    = "text"
	MessageTypeImage   = "image"
	MessageTypeVoice   = "voice"
	MessageTypeFile    = "file"
	MessageTypeVideo   = "video"
	MessageTypeLink    = "link"
	MessageTypeSphFeed = "sphfeed"
	MessageTypeMixed   = "mixed"
)

type Text struct {
	Content string `json:"content"`
}

type Image struct {
	SDKFileID string `json:"sdkfileid"`
	MD5Sum    string `json:"md5sum"`
	FileSize  int64  `json:"filesize"`
}

type Voice struct {
	SDKFileID  string `json:"sdkfileid"`
	VoiceSize  int64  `json:"voice_size"`
	PlayLength int64  `json:"play_length"`
	MD5Sum     string `json:"md5sum"`
}

type File struct {
	SDKFileID string `json:"sdkfileid"`
	MD5Sum    string `json:"md5sum"`
	FileName  string `json:"filename"`
	FileExt   string `json:"fileext"`
	FileSize  int64  `json:"filesize"`
}

type Video struct {
	SDKFileID  string `json:"sdkfileid"`
	FileSize   int64  `json:"filesize"`
	PlayLength int64  `json:"play_length"`
	MD5Sum     string `json:"md5sum"`
}

type Link struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	LinkURL     string `json:"link_url"`
	ImageURL    string `json:"image_url"`
}

type SphFeed struct {
	FeedType int64  `json:"feed_type"`
	SphName  string `json:"sph_name"`
	FeedDesc string `json:"feed_desc"`
}

type MixedItem struct {
	Type      string `json:"type"`
	Content   string `json:"content,omitempty"`
	SDKFileID string `json:"sdkfileid,omitempty"`
	MD5Sum    string `json:"md5sum,omitempty"`
	FileSize  int64  `json:"filesize,omitempty"`

	MessageID string `json:"-"`
}

type Mixed struct {
	Items []MixedItem `json:"item"`
}

// Message follows the WeCom conversation-archive message envelope. Fields
// tagged json:"-" are local database bookkeeping and never cross the API.
type Message struct {
	MsgID   string   `json:"msgid"`
	Action  string   `json:"action"`
	From    string   `json:"from"`
	ToList  []string `json:"tolist"`
	RoomID  string   `json:"roomid"`
	MsgTime int64    `json:"msgtime"`
	MsgType string   `json:"msgtype"`

	Text    *Text    `json:"text,omitempty"`
	Image   *Image   `json:"image,omitempty"`
	Voice   *Voice   `json:"voice,omitempty"`
	File    *File    `json:"file,omitempty"`
	Video   *Video   `json:"video,omitempty"`
	Link    *Link    `json:"link,omitempty"`
	SphFeed *SphFeed `json:"sphfeed,omitempty"`
	Mixed   *Mixed   `json:"mixed,omitempty"`

	Sequence       int64  `json:"-"`
	ConversationID string `json:"-"`
	Outgoing       bool   `json:"-"`
}
