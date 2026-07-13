// Portions of this module are adapted from jackwener/wx-cli (Apache-2.0) and modified for Webox.
// See LICENSES/Apache-2.0.txt and THIRD_PARTY_NOTICES.md.
// The agent keeps this code in-process so message state and HTTP cursors stay owned by webox.
use aes::cipher::{generic_array::GenericArray, BlockDecrypt, KeyInit};
use aes::{Aes128, Aes256};
use anyhow::{anyhow, bail, Context, Result};
use cbc::cipher::{BlockDecryptMut, KeyIvInit};
use cbc::Decryptor;
use rusqlite::{Connection, OptionalExtension};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::collections::{BTreeMap, HashMap, HashSet};
use std::fs;
use std::io::{Read, Seek, SeekFrom, Write};
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{mpsc, Arc, Mutex, OnceLock};
use std::time::{SystemTime, UNIX_EPOCH};

const HEX_PATTERN_LEN: usize = 96;
const CHUNK_SIZE: usize = 2 * 1024 * 1024;
const PAGE_SZ: usize = 4096;
const SALT_SZ: usize = 16;
const RESERVE_SZ: usize = 80;
const WAL_HDR_SZ: usize = 32;
const WAL_FRAME_HDR: usize = 24;
const SQLITE_HDR: &[u8] = b"SQLite format 3\x00";
const V2_IMAGE_MAGIC: [u8; 6] = [0x07, 0x08, b'V', b'2', 0x08, 0x07];
const V1_IMAGE_MAGIC: [u8; 6] = [0x07, 0x08, b'V', b'1', 0x08, 0x07];
const V2_IMAGE_HEADER_SIZE: usize = 15;
const MAX_LOCAL_MEDIA_BYTES: u64 = 256 * 1024 * 1024;

type Aes256CbcDec = Decryptor<Aes256>;
type Block = aes::cipher::Block<Aes256>;

#[derive(Debug, Clone)]
struct KeyEntry {
    db_name: String,
    enc_key: String,
}

#[derive(Debug, Clone)]
pub struct InitData {
    pub db_dir: PathBuf,
    pub wxid: String,
    pub keys: HashMap<String, String>,
}

#[derive(Clone, Copy, Debug, Default, Deserialize, Eq, Ord, PartialEq, PartialOrd, Serialize)]
pub struct MessagePosition {
    pub create_time: i64,
    pub local_id: i64,
}

pub type MessagePositions = BTreeMap<String, BTreeMap<String, MessagePosition>>;
pub type RoomMessagePositions = BTreeMap<String, MessagePosition>;

#[derive(Debug, Clone, Serialize)]
pub struct PollData {
    pub messages: Vec<Value>,
    pub new_state: MessagePositions,
}

#[derive(Debug, Clone)]
pub struct MediaFile {
    pub data: Vec<u8>,
    pub content_type: String,
    pub filename: String,
}

#[derive(Debug, Clone, Copy)]
struct ImageKeyMaterial {
    aes_key: [u8; 16],
    xor_key: u8,
}

#[derive(Debug, Clone)]
pub struct Recipient {
    pub username: String,
    pub search_term: String,
}

#[derive(Debug, Clone)]
pub struct ContactIdentity {
    pub display: String,
    pub has_remark: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
struct MtimeEntry {
    db_mt: u64,
    wal_mt: u64,
    path: String,
}

#[derive(Debug, Clone)]
struct CacheEntry {
    db_mtime: u64,
    wal_mtime: u64,
    decrypted_path: PathBuf,
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct ReferMsg {
    chatusr: String,
    displayname: String,
    msgid: String,
    msgtype: String,
    content: String,
    body: Value,
}

struct DbCache {
    db_dir: PathBuf,
    cache_dir: PathBuf,
    mtime_file: PathBuf,
    all_keys: HashMap<String, String>,
    inner: HashMap<String, CacheEntry>,
}

#[derive(Clone)]
struct MessageIndex {
    msg_db_keys: Vec<String>,
}

#[derive(Debug, Clone)]
struct MessageShard {
    rel_key: String,
    path: PathBuf,
    table: String,
    max_ts: i64,
}

pub fn detect_db_storage() -> Option<PathBuf> {
    let base = wx_home().join("xwechat_files");
    let mut candidates = Vec::new();
    if let Ok(entries) = fs::read_dir(base) {
        for entry in entries.flatten() {
            let storage = entry.path().join("db_storage");
            if storage.is_dir() {
                candidates.push(storage);
            }
        }
    }
    candidates.sort_by_key(|p| latest_db_mtime(p).unwrap_or(UNIX_EPOCH));
    candidates.into_iter().next_back()
}

pub fn account_id_from_db_dir(db_dir: &Path) -> Option<String> {
    let account_dir = db_dir.parent()?;
    let raw = account_dir.file_name()?.to_string_lossy();
    let normalized = normalize_wxid(&raw);
    (!normalized.is_empty()).then_some(normalized)
}

pub fn init_from_memory() -> Result<InitData> {
    let db_dir = detect_db_storage().ok_or_else(|| anyhow!("未找到微信 db_storage 目录"))?;
    let entries = scan_keys(&db_dir)?;
    if entries.is_empty() {
        bail!("未从内存提取到有效 Message Key");
    }
    let keys = entries
        .iter()
        .map(|entry| (entry.db_name.clone(), entry.enc_key.clone()))
        .collect();
    let wxid = account_id_from_db_dir(&db_dir)
        .ok_or_else(|| anyhow!("无法从微信数据库目录识别当前账号"))?;
    Ok(InitData { db_dir, wxid, keys })
}

pub fn poll_new_messages(
    db_dir: PathBuf,
    keys: HashMap<String, String>,
    state: MessagePositions,
    started_at: i64,
    limit: usize,
    cache_dir: PathBuf,
) -> Result<PollData> {
    let mut db = DbCache::new(db_dir, cache_dir, keys)?;
    let index = message_index(&db);
    q_new_messages(&mut db, &index, state, started_at, limit)
}

pub fn baseline_message_positions(
    db_dir: PathBuf,
    keys: HashMap<String, String>,
    cache_dir: PathBuf,
    started_at: i64,
) -> Result<MessagePositions> {
    let mut db = DbCache::new(db_dir, cache_dir, keys)?;
    let index = message_index(&db);
    let sessions = load_session_state(&mut db)?;
    let mut positions = BTreeMap::new();
    for username in sessions.keys() {
        let mut room = BTreeMap::new();
        for shard in find_msg_shards(&mut db, &index, username)? {
            room.insert(
                shard.rel_key,
                max_message_position(&shard.path, &shard.table)?.unwrap_or(MessagePosition {
                    create_time: started_at,
                    local_id: 0,
                }),
            );
        }
        positions.insert(username.clone(), room);
    }
    Ok(positions)
}

pub fn current_session_state(
    db_dir: PathBuf,
    keys: HashMap<String, String>,
    cache_dir: PathBuf,
) -> Result<HashMap<String, i64>> {
    let mut db = DbCache::new(db_dir, cache_dir, keys)?;
    load_session_state(&mut db)
}

pub fn has_outgoing_text(
    db_dir: PathBuf,
    keys: HashMap<String, String>,
    cache_dir: PathBuf,
    roomid: &str,
    positions: &RoomMessagePositions,
    text: &str,
) -> Result<bool> {
    has_outgoing_payload(
        db_dir,
        keys,
        cache_dir,
        roomid,
        positions,
        |local_type, content| {
            is_text_message(local_type)
                && strip_group_prefix(content, roomid.ends_with("@chatroom")) == text
        },
    )
}

pub fn outgoing_text_contains(
    db_dir: PathBuf,
    keys: HashMap<String, String>,
    cache_dir: PathBuf,
    roomid: &str,
    needle: &str,
) -> Result<bool> {
    let mut db = DbCache::new(db_dir, cache_dir, keys)?;
    let index = message_index(&db);
    for shard in find_msg_shards(&mut db, &index, roomid)? {
        let conn = Connection::open(&shard.path)?;
        let sql = format!(
            "SELECT local_type, message_content, WCDB_CT_message_content
             FROM [{}] WHERE status = 2 AND origin_source = 1
             ORDER BY create_time DESC, local_id DESC",
            shard.table
        );
        let mut stmt = conn.prepare(&sql)?;
        let rows = stmt.query_map([], |row| {
            Ok((
                row.get::<_, i64>(0)?,
                get_content_bytes(row, 1),
                row.get::<_, i64>(2).unwrap_or(0),
            ))
        })?;
        for row in rows.flatten() {
            if is_text_message(row.0) && decompress_message(&row.1, row.2).contains(needle) {
                return Ok(true);
            }
        }
    }
    Ok(false)
}

pub fn room_message_positions(
    db_dir: PathBuf,
    keys: HashMap<String, String>,
    cache_dir: PathBuf,
    roomid: &str,
) -> Result<RoomMessagePositions> {
    let mut db = DbCache::new(db_dir, cache_dir, keys)?;
    let index = message_index(&db);
    let mut positions = BTreeMap::new();
    for shard in find_msg_shards(&mut db, &index, roomid)? {
        positions.insert(
            shard.rel_key,
            max_message_position(&shard.path, &shard.table)?.unwrap_or_default(),
        );
    }
    Ok(positions)
}

fn has_outgoing_payload(
    db_dir: PathBuf,
    keys: HashMap<String, String>,
    cache_dir: PathBuf,
    roomid: &str,
    positions: &RoomMessagePositions,
    matches: impl Fn(i64, &str) -> bool,
) -> Result<bool> {
    let mut db = DbCache::new(db_dir, cache_dir, keys)?;
    let index = message_index(&db);
    for shard in find_msg_shards(&mut db, &index, roomid)? {
        let position = positions.get(&shard.rel_key).copied().unwrap_or_default();
        let conn = Connection::open(&shard.path)?;
        let sql = format!(
            "SELECT local_type, message_content, WCDB_CT_message_content
             FROM [{}]
             WHERE ((create_time > ?1) OR (create_time = ?2 AND local_id > ?3))
               AND status = 2 AND origin_source = 1
             ORDER BY create_time DESC, local_id DESC LIMIT 100",
            shard.table
        );
        let mut stmt = conn.prepare(&sql)?;
        let rows = stmt.query_map(
            rusqlite::params![
                position.create_time,
                position.create_time,
                position.local_id
            ],
            |row| {
                Ok((
                    row.get::<_, i64>(0)?,
                    get_content_bytes(row, 1),
                    row.get::<_, i64>(2).unwrap_or(0),
                ))
            },
        )?;
        for row in rows.flatten() {
            let content = decompress_message(&row.1, row.2);
            if matches(row.0, &content) {
                return Ok(true);
            }
        }
    }
    Ok(false)
}

pub fn read_media(
    db_dir: PathBuf,
    keys: HashMap<String, String>,
    cache_dir: PathBuf,
    roomid: &str,
    msgid: &str,
) -> Result<Option<MediaFile>> {
    if let Some(media) = read_image_media(
        db_dir.clone(),
        keys.clone(),
        cache_dir.clone(),
        roomid,
        msgid,
    )? {
        return Ok(Some(media));
    }
    if let Some(media) = read_video_media(
        db_dir.clone(),
        keys.clone(),
        cache_dir.clone(),
        roomid,
        msgid,
    )? {
        return Ok(Some(media));
    }
    read_file_media(db_dir, keys, cache_dir, roomid, msgid)
}

pub fn read_image_media(
    db_dir: PathBuf,
    keys: HashMap<String, String>,
    cache_dir: PathBuf,
    roomid: &str,
    msgid: &str,
) -> Result<Option<MediaFile>> {
    let server_id = match msgid.trim().parse::<i64>() {
        Ok(value) if value > 0 => value,
        _ => return Ok(None),
    };
    let roomid = roomid.trim();
    if roomid.is_empty() {
        return Ok(None);
    }
    let mut db = DbCache::new(db_dir.clone(), cache_dir, keys)?;
    let index = message_index(&db);
    let shards = find_msg_shards(&mut db, &index, roomid)?;
    let account_dir = db_dir
        .parent()
        .ok_or_else(|| anyhow!("db_storage 目录缺少账号父目录"))?
        .to_path_buf();
    let attach_root = account_dir.join("msg").join("attach");
    for shard in shards {
        if let Some((local_id, content, create_time, local_type)) =
            media_message_content(&shard.path, &shard.table, server_id, &[3, 47, 49])?
        {
            if local_type == 47 {
                if let Some(media) = find_emotion_media(&account_dir, &content)? {
                    return Ok(Some(media));
                }
                continue;
            }
            if let Some(media) = find_resource_image_file(
                &mut db,
                &account_dir,
                &attach_root,
                roomid,
                local_id,
                create_time,
                local_type,
            )? {
                return Ok(Some(media));
            }
            if let Some(media) = find_image_file(&account_dir, local_id, create_time, &content)? {
                return Ok(Some(media));
            }
        }
    }
    Ok(None)
}

fn read_video_media(
    db_dir: PathBuf,
    keys: HashMap<String, String>,
    cache_dir: PathBuf,
    roomid: &str,
    msgid: &str,
) -> Result<Option<MediaFile>> {
    let server_id = match msgid.trim().parse::<i64>() {
        Ok(value) if value > 0 => value,
        _ => return Ok(None),
    };
    let roomid = roomid.trim();
    if roomid.is_empty() {
        return Ok(None);
    }
    let mut db = DbCache::new(db_dir.clone(), cache_dir, keys)?;
    let index = message_index(&db);
    let shards = find_msg_shards(&mut db, &index, roomid)?;
    let account_dir = db_dir
        .parent()
        .ok_or_else(|| anyhow!("db_storage 目录缺少账号父目录"))?
        .to_path_buf();
    for shard in shards {
        if let Some((local_id, content, create_time, _local_type)) =
            media_message_content(&shard.path, &shard.table, server_id, &[43])?
        {
            if let Some(media) = find_video_file(&account_dir, local_id, create_time, &content)? {
                return Ok(Some(media));
            }
        }
    }
    Ok(None)
}

fn read_file_media(
    db_dir: PathBuf,
    keys: HashMap<String, String>,
    cache_dir: PathBuf,
    roomid: &str,
    msgid: &str,
) -> Result<Option<MediaFile>> {
    let server_id = match msgid.trim().parse::<i64>() {
        Ok(value) if value > 0 => value,
        _ => return Ok(None),
    };
    let roomid = roomid.trim();
    if roomid.is_empty() {
        return Ok(None);
    }
    let mut db = DbCache::new(db_dir.clone(), cache_dir, keys)?;
    let index = message_index(&db);
    let shards = find_msg_shards(&mut db, &index, roomid)?;
    let account_dir = db_dir
        .parent()
        .ok_or_else(|| anyhow!("db_storage 目录缺少账号父目录"))?;
    for shard in shards {
        let Some((_local_id, content, _create_time, _local_type)) =
            media_message_content(&shard.path, &shard.table, server_id, &[49])?
        else {
            continue;
        };
        let content = strip_group_prefix(&content, roomid.ends_with("@chatroom"));
        if appmsg_type(content) != Some(6) {
            continue;
        }
        let filename = appmsg_title(content);
        if filename.is_empty() {
            continue;
        }
        let Some(path) = find_file_by_name(&account_dir.join("msg").join("file"), &filename)?
        else {
            continue;
        };
        let metadata = fs::metadata(&path)?;
        if metadata.len() == 0 || metadata.len() > MAX_LOCAL_MEDIA_BYTES {
            continue;
        }
        return Ok(Some(MediaFile {
            data: fs::read(path)?,
            content_type: "application/octet-stream".to_string(),
            filename,
        }));
    }
    Ok(None)
}

fn recipient_display_name(username: &str, nick: &str, remark: &str, alias: &str) -> String {
    let remark = remark.trim();
    if !remark.is_empty() {
        return remark.to_string();
    }
    let nick = nick.trim();
    if !nick.is_empty() {
        return nick.to_string();
    }
    let alias = alias.trim();
    if !alias.is_empty() {
        return alias.to_string();
    }
    username.trim().to_string()
}

fn recipient_from_remark(username: String, remark: String) -> Option<Recipient> {
    let search_term = remark.trim();
    (!search_term.is_empty()).then(|| Recipient {
        username,
        search_term: search_term.to_string(),
    })
}

pub fn contact_identity(
    db_dir: PathBuf,
    keys: HashMap<String, String>,
    cache_dir: PathBuf,
    raw: &str,
) -> Result<Option<ContactIdentity>> {
    let username = raw.trim();
    if username.is_empty() {
        return Ok(None);
    }
    let mut db = DbCache::new(db_dir, cache_dir, keys)?;
    let Some(path) = db.get("contact/contact.db")? else {
        return Ok(None);
    };
    let conn = Connection::open(path)?;
    conn.query_row(
        "SELECT nick_name, remark, alias FROM contact WHERE delete_flag=0 AND username=?1 LIMIT 1",
        [username],
        |row| {
            let nick = row.get::<_, String>(0).unwrap_or_default();
            let remark = row.get::<_, String>(1).unwrap_or_default();
            let alias = row.get::<_, String>(2).unwrap_or_default();
            Ok(ContactIdentity {
                display: recipient_display_name(username, &nick, &remark, &alias),
                has_remark: !remark.trim().is_empty(),
            })
        },
    )
    .optional()
    .map_err(Into::into)
}

pub fn resolve_recipient_by_username(
    db_dir: PathBuf,
    keys: HashMap<String, String>,
    cache_dir: PathBuf,
    raw: &str,
    current_user_id: &str,
) -> Result<Option<Recipient>> {
    let username = raw.trim();
    if username.is_empty() {
        return Ok(None);
    }
    if username == "filehelper" {
        return Ok(Some(Recipient {
            username: username.to_string(),
            search_term: "文件传输助手".to_string(),
        }));
    }
    let mut db = DbCache::new(db_dir, cache_dir, keys)?;
    let Some(path) = db.get("contact/contact.db")? else {
        return Ok(None);
    };
    let conn = Connection::open(path)?;
    let contact = conn
        .query_row(
        "SELECT username, nick_name, remark, alias FROM contact WHERE delete_flag=0 AND username=?1 LIMIT 1",
        [username],
        |row| {
            Ok((
                row.get::<_, String>(0)?,
                row.get::<_, String>(1).unwrap_or_default(),
                row.get::<_, String>(2).unwrap_or_default(),
                row.get::<_, String>(3).unwrap_or_default(),
            ))
        },
    )
    .optional()?;
    let Some((username, nick, remark, alias)) = contact else {
        return Ok(None);
    };
    if username == current_user_id {
        return Ok(Some(Recipient {
            search_term: recipient_display_name(&username, &nick, &remark, &alias),
            username,
        }));
    }
    let Some(recipient) = recipient_from_remark(username, remark) else {
        bail!("联系人或群聊必须设置唯一备注作为发送搜索词");
    };
    let duplicate: Option<String> = conn
        .query_row(
            "SELECT username FROM contact
             WHERE delete_flag=0 AND username<>?2
               AND (username=?1 OR nick_name=?1 OR remark=?1 OR alias=?1)
             LIMIT 1",
            [&recipient.search_term, &recipient.username],
            |row| row.get(0),
        )
        .optional()?;
    if duplicate.is_some() {
        bail!("联系人搜索词不唯一，请先设置唯一备注");
    }
    Ok(Some(recipient))
}

fn wx_home() -> PathBuf {
    PathBuf::from(
        std::env::var("WEBOX_WX_HOME")
            .or_else(|_| std::env::var("WEBOX_HOME"))
            .unwrap_or_else(|_| "/webox/state/home".to_string()),
    )
}

fn latest_db_mtime(dir: &Path) -> Option<SystemTime> {
    let mut latest = None;
    let entries = fs::read_dir(dir).ok()?;
    for entry in entries.flatten() {
        let path = entry.path();
        let mtime = if path.is_dir() {
            latest_db_mtime(&path).unwrap_or(UNIX_EPOCH)
        } else if path.extension().and_then(|s| s.to_str()) == Some("db") {
            entry
                .metadata()
                .and_then(|m| m.modified())
                .unwrap_or(UNIX_EPOCH)
        } else {
            continue;
        };
        latest = Some(latest.map_or(mtime, |cur| if mtime > cur { mtime } else { cur }));
    }
    latest
}

fn scan_keys(db_dir: &Path) -> Result<Vec<KeyEntry>> {
    let pids = find_wechat_pids();
    if pids.is_empty() {
        bail!("找不到 WeChat 进程，请确认 WeChat 正在运行");
    }
    let db_salts = collect_db_salts(db_dir);
    if db_salts.is_empty() {
        bail!("未找到加密数据库");
    }

    let mut raw_keys: Vec<(String, String)> = Vec::new();
    let mut readable_processes = 0usize;
    let mut scanned_regions = 0usize;
    let mut scanned_bytes = 0usize;
    for pid in pids {
        let Ok(regions) = parse_maps(pid) else {
            continue;
        };
        let mem_path = format!("/proc/{pid}/mem");
        let Ok(mut mem_file) = fs::File::open(&mem_path) else {
            continue;
        };
        readable_processes += 1;
        scanned_regions += regions.len();
        for (start, end) in &regions {
            scanned_bytes += scan_region(&mut mem_file, *start, *end, &mut raw_keys);
        }
    }

    let mut entries = Vec::new();
    for (key_hex, salt_hex) in &raw_keys {
        for (db_salt, db_name) in &db_salts {
            if salt_hex == db_salt
                && !entries
                    .iter()
                    .any(|entry: &KeyEntry| entry.db_name == *db_name)
            {
                entries.push(KeyEntry {
                    db_name: db_name.clone(),
                    enc_key: key_hex.clone(),
                });
            }
        }
    }
    if entries.is_empty() {
        bail!(
            "未从内存提取到有效 Message Key: readable_processes={readable_processes}, \
             scanned_regions={scanned_regions}, scanned_bytes={scanned_bytes}, \
             key_candidates={}, database_salts={}",
            raw_keys.len(),
            db_salts.len()
        );
    }
    Ok(entries)
}

fn find_wechat_pids() -> Vec<u32> {
    let mut pids = Vec::new();
    let Ok(proc_dir) = fs::read_dir("/proc") else {
        return pids;
    };
    for entry in proc_dir.flatten() {
        let name = entry.file_name();
        let name_str = name.to_string_lossy();
        if !name_str.chars().all(|c| c.is_ascii_digit()) {
            continue;
        }
        let Ok(pid) = name_str.parse::<u32>() else {
            continue;
        };
        let comm = fs::read_to_string(format!("/proc/{pid}/comm"))
            .unwrap_or_default()
            .trim()
            .to_lowercase();
        let cmdline = fs::read(format!("/proc/{pid}/cmdline"))
            .ok()
            .map(|buf| {
                String::from_utf8_lossy(&buf)
                    .replace('\0', " ")
                    .to_lowercase()
            })
            .unwrap_or_default();
        if comm == "wechat"
            || comm == "weixin"
            || comm == "wechatappex"
            || cmdline.contains("/wechat/")
            || cmdline.contains("wechatappex")
        {
            pids.push(pid);
        }
    }
    pids.sort_unstable();
    pids.dedup();
    pids
}

fn parse_maps(pid: u32) -> Result<Vec<(u64, u64)>> {
    let maps_path = format!("/proc/{pid}/maps");
    let content =
        fs::read_to_string(&maps_path).with_context(|| format!("读取 {maps_path} 失败"))?;
    let mut regions = Vec::new();
    for line in content.lines() {
        let parts: Vec<&str> = line.splitn(2, ' ').collect();
        if parts.len() < 2 {
            continue;
        }
        let perms = parts[1].trim_start();
        if !perms.starts_with("rw") {
            continue;
        }
        let addr_parts: Vec<&str> = parts[0].splitn(2, '-').collect();
        if addr_parts.len() != 2 {
            continue;
        }
        if let (Ok(start), Ok(end)) = (
            u64::from_str_radix(addr_parts[0], 16),
            u64::from_str_radix(addr_parts[1], 16),
        ) {
            regions.push((start, end));
        }
    }
    Ok(regions)
}

fn scan_region(
    mem: &mut fs::File,
    start: u64,
    end: u64,
    results: &mut Vec<(String, String)>,
) -> usize {
    let total_len = (end - start) as usize;
    let overlap = HEX_PATTERN_LEN + 3;
    let mut offset = 0usize;
    let mut scanned_bytes = 0usize;
    while offset < total_len {
        let chunk_size = std::cmp::min(CHUNK_SIZE, total_len - offset);
        let addr = start + offset as u64;
        if mem.seek(SeekFrom::Start(addr)).is_err() {
            break;
        }
        let mut buf = vec![0u8; chunk_size];
        match mem.read(&mut buf) {
            Ok(n) if n > 0 => {
                buf.truncate(n);
                scanned_bytes += n;
                search_pattern(&buf, results);
            }
            _ => {}
        }
        if chunk_size > overlap {
            offset += chunk_size - overlap;
        } else {
            offset += chunk_size;
        }
    }
    scanned_bytes
}

fn search_pattern(buf: &[u8], results: &mut Vec<(String, String)>) {
    let total = HEX_PATTERN_LEN + 3;
    if buf.len() < total {
        return;
    }
    let mut i = 0;
    while i + total <= buf.len() {
        if buf[i] != b'x' || buf[i + 1] != b'\'' {
            i += 1;
            continue;
        }
        let hex_start = i + 2;
        let all_hex = buf[hex_start..hex_start + HEX_PATTERN_LEN]
            .iter()
            .all(|c| c.is_ascii_hexdigit());
        if !all_hex || buf[hex_start + HEX_PATTERN_LEN] != b'\'' {
            i += 1;
            continue;
        }
        let key_hex = String::from_utf8_lossy(&buf[hex_start..hex_start + 64]).to_lowercase();
        let salt_hex = String::from_utf8_lossy(&buf[hex_start + 64..hex_start + 96]).to_lowercase();
        if !results.iter().any(|(k, s)| k == &key_hex && s == &salt_hex) {
            results.push((key_hex, salt_hex));
        }
        i += total;
    }
}

fn collect_db_salts(db_dir: &Path) -> Vec<(String, String)> {
    let mut result = Vec::new();
    collect_recursive(db_dir, db_dir, &mut result);
    result
}

fn collect_recursive(base: &Path, dir: &Path, out: &mut Vec<(String, String)>) {
    let Ok(entries) = fs::read_dir(dir) else {
        return;
    };
    for entry in entries.flatten() {
        let path = entry.path();
        if path.is_dir() {
            collect_recursive(base, &path, out);
        } else if path.extension().map(|e| e == "db").unwrap_or(false) {
            if let Some(salt) = read_db_salt(&path) {
                if let Ok(rel) = path.strip_prefix(base) {
                    out.push((salt, rel.to_string_lossy().replace('\\', "/")));
                }
            }
        }
    }
}

fn read_db_salt(path: &Path) -> Option<String> {
    let mut buf = [0u8; 16];
    let mut f = fs::File::open(path).ok()?;
    f.read_exact(&mut buf).ok()?;
    if &buf[..15] == b"SQLite format 3" {
        return None;
    }
    Some(hex_encode(&buf))
}

impl DbCache {
    fn new(db_dir: PathBuf, cache_dir: PathBuf, all_keys: HashMap<String, String>) -> Result<Self> {
        fs::create_dir_all(&cache_dir)?;
        let mtime_file = cache_dir.join("_mtimes.json");
        let mut cache = Self {
            db_dir,
            cache_dir,
            mtime_file,
            all_keys,
            inner: HashMap::new(),
        };
        cache.load_persistent();
        Ok(cache)
    }

    fn cache_file_path(&self, rel_key: &str) -> PathBuf {
        let hash = format!("{:x}", md5::compute(rel_key.as_bytes()));
        self.cache_dir.join(format!("{hash}.db"))
    }

    fn load_persistent(&mut self) {
        let Ok(content) = fs::read_to_string(&self.mtime_file) else {
            return;
        };
        let Ok(saved) = serde_json::from_str::<HashMap<String, MtimeEntry>>(&content) else {
            return;
        };
        for (rel_key, entry) in saved {
            let dec_path = PathBuf::from(&entry.path);
            if !dec_path.exists() {
                continue;
            }
            let db_path = self.db_path(&rel_key);
            if mtime_nanos(&db_path) == entry.db_mt {
                self.inner.insert(
                    rel_key,
                    CacheEntry {
                        db_mtime: entry.db_mt,
                        wal_mtime: entry.wal_mt,
                        decrypted_path: dec_path,
                    },
                );
            }
        }
    }

    fn save_persistent(&self) {
        let data: HashMap<String, MtimeEntry> = self
            .inner
            .iter()
            .map(|(k, v)| {
                (
                    k.clone(),
                    MtimeEntry {
                        db_mt: v.db_mtime,
                        wal_mt: v.wal_mtime,
                        path: v.decrypted_path.to_string_lossy().into_owned(),
                    },
                )
            })
            .collect();
        if let Ok(json) = serde_json::to_string_pretty(&data) {
            let _ = fs::write(&self.mtime_file, json);
        }
    }

    fn db_path(&self, rel_key: &str) -> PathBuf {
        self.db_dir
            .join(rel_key.replace(['\\', '/'], std::path::MAIN_SEPARATOR_STR))
    }

    fn get(&mut self, rel_key: &str) -> Result<Option<PathBuf>> {
        let Some(enc_key_hex) = self.all_keys.get(rel_key).cloned() else {
            return Ok(None);
        };
        let db_path = self.db_path(rel_key);
        if !db_path.exists() {
            return Ok(None);
        }
        let wal_path = wal_path_for(&db_path);
        let db_mt = mtime_nanos(&db_path);
        let wal_mt = if wal_path.exists() {
            mtime_nanos(&wal_path)
        } else {
            0
        };
        let enc_key_bytes =
            hex_to_32bytes(&enc_key_hex).with_context(|| format!("密钥格式错误: {rel_key}"))?;

        if let Some(entry) = self.inner.get(rel_key).cloned() {
            if entry.db_mtime == db_mt && entry.decrypted_path.exists() {
                if entry.wal_mtime == wal_mt {
                    return Ok(Some(entry.decrypted_path));
                }
                if wal_path.exists() {
                    apply_wal(&wal_path, &entry.decrypted_path, &enc_key_bytes)?;
                }
                self.inner.insert(
                    rel_key.to_string(),
                    CacheEntry {
                        db_mtime: db_mt,
                        wal_mtime: wal_mt,
                        decrypted_path: entry.decrypted_path.clone(),
                    },
                );
                self.save_persistent();
                return Ok(Some(entry.decrypted_path));
            }
        }

        let out_path = self.cache_file_path(rel_key);
        full_decrypt(&db_path, &out_path, &enc_key_bytes)?;
        if wal_path.exists() {
            apply_wal(&wal_path, &out_path, &enc_key_bytes)?;
        }
        self.inner.insert(
            rel_key.to_string(),
            CacheEntry {
                db_mtime: db_mt,
                wal_mtime: wal_mt,
                decrypted_path: out_path.clone(),
            },
        );
        self.save_persistent();
        Ok(Some(out_path))
    }
}

fn message_index(db: &DbCache) -> MessageIndex {
    let mut msg_db_keys: Vec<String> = db
        .all_keys
        .keys()
        .filter(|key| key.starts_with("message/") && is_message_shard_key(key))
        .cloned()
        .collect();
    msg_db_keys.sort();
    MessageIndex { msg_db_keys }
}

fn q_new_messages(
    db: &mut DbCache,
    index: &MessageIndex,
    mut state: MessagePositions,
    started_at: i64,
    limit: usize,
) -> Result<PollData> {
    let session_ts_map = load_session_state(db)?;
    let changed: Vec<String> = session_ts_map
        .iter()
        .filter(|(uname, ts)| {
            let last_known = state
                .get(*uname)
                .and_then(|streams| streams.values().max())
                .map(|position| position.create_time)
                .unwrap_or(started_at);
            **ts >= last_known
        })
        .map(|(uname, _)| uname.clone())
        .collect();
    if changed.is_empty() {
        return Ok(PollData {
            messages: Vec::new(),
            new_state: state,
        });
    }

    let per_table_limit = limit.saturating_mul(4).clamp(100, 2_000);
    let mut events = Vec::new();

    for uname in &changed {
        let shards = find_msg_shards(db, index, uname)?;
        if shards.is_empty() {
            continue;
        }
        let is_group = uname.ends_with("@chatroom");
        for shard in &shards {
            let position = state
                .get(uname)
                .and_then(|streams| streams.get(&shard.rel_key))
                .copied()
                .unwrap_or(MessagePosition {
                    create_time: started_at,
                    local_id: 0,
                });
            let rows = query_new_table(
                &shard.path,
                &shard.table,
                &shard.rel_key,
                uname,
                is_group,
                position,
                per_table_limit,
            )
            .with_context(|| format!("query message table {}", shard.table))?;
            events.extend(rows);
        }
    }

    events.sort_by(|a, b| {
        a.position
            .cmp(&b.position)
            .then_with(|| a.room.cmp(&b.room))
            .then_with(|| a.shard.cmp(&b.shard))
    });
    let scan_limit = limit.saturating_mul(10).clamp(200, 5_000);
    let mut messages = Vec::new();
    for event in events.into_iter().take(scan_limit) {
        state
            .entry(event.room)
            .or_default()
            .insert(event.shard, event.position);
        if let Some(message) = event.message {
            messages.push(message);
            if messages.len() >= limit {
                break;
            }
        }
    }

    Ok(PollData {
        messages,
        new_state: state,
    })
}

#[derive(Debug)]
struct MessageEvent {
    room: String,
    shard: String,
    position: MessagePosition,
    message: Option<Value>,
}

fn load_session_state(db: &mut DbCache) -> Result<HashMap<String, i64>> {
    let session_path = db
        .get("session/session.db")?
        .ok_or_else(|| anyhow!("无法解密 session.db"))?;
    let conn = Connection::open(session_path)?;
    let mut stmt =
        conn.prepare("SELECT username, last_timestamp FROM SessionTable WHERE last_timestamp > 0")?;
    let sessions = stmt
        .query_map([], |row| {
            Ok((row.get::<_, String>(0)?, row.get::<_, i64>(1).unwrap_or(0)))
        })?
        .filter_map(|row| row.ok())
        .collect();
    Ok(sessions)
}

fn find_msg_shards(
    db: &mut DbCache,
    index: &MessageIndex,
    username: &str,
) -> Result<Vec<MessageShard>> {
    let table_name = format!("Msg_{:x}", md5::compute(username.as_bytes()));
    let mut results = Vec::new();
    for rel_key in &index.msg_db_keys {
        let Some(path) = db.get(rel_key)? else {
            continue;
        };
        let conn = Connection::open(&path)?;
        let table_exists: Option<i64> = conn
            .query_row(
                "SELECT 1 FROM sqlite_master WHERE type='table' AND name=?",
                [&table_name],
                |row| row.get(0),
            )
            .ok()
            .flatten();
        if table_exists.is_none() {
            continue;
        }
        let max_ts: Option<i64> = conn
            .query_row(
                &format!("SELECT MAX(create_time) FROM [{}]", table_name),
                [],
                |row| row.get(0),
            )
            .ok()
            .flatten();
        if let Some(ts) = max_ts {
            results.push(MessageShard {
                rel_key: rel_key.clone(),
                path,
                table: table_name.clone(),
                max_ts: ts,
            });
        }
    }
    results.sort_by_key(|s| std::cmp::Reverse(s.max_ts));
    Ok(results)
}

fn query_new_table(
    db_path: &Path,
    table: &str,
    shard: &str,
    username: &str,
    is_group: bool,
    position: MessagePosition,
    limit: usize,
) -> Result<Vec<MessageEvent>> {
    let conn = Connection::open(db_path)?;
    let id2u = load_id2u(&conn);
    let sql = format!(
        "SELECT local_id, server_id, local_type, create_time, real_sender_id,
                message_content, WCDB_CT_message_content, status, origin_source
         FROM [{}]
         WHERE create_time > ? OR (create_time = ? AND local_id > ?)
         ORDER BY create_time ASC, local_id ASC LIMIT ?",
        table
    );
    let mut stmt = conn.prepare(&sql)?;
    let rows: Vec<_> = stmt
        .query_map(
            rusqlite::params![
                position.create_time,
                position.create_time,
                position.local_id,
                limit as i64
            ],
            |row| {
                Ok((
                    row.get::<_, i64>(0)?,
                    row.get::<_, i64>(1)?,
                    row.get::<_, i64>(2)?,
                    row.get::<_, i64>(3)?,
                    row.get::<_, i64>(4)?,
                    get_content_bytes(row, 5),
                    row.get::<_, i64>(6).unwrap_or(0),
                    row.get::<_, i64>(7).unwrap_or(0),
                    row.get::<_, i64>(8).unwrap_or(0),
                ))
            },
        )?
        .filter_map(|row| row.ok())
        .collect();

    let mut out = Vec::new();
    for (
        local_id,
        server_id,
        local_type,
        ts,
        real_sender_id,
        content_bytes,
        ct,
        status,
        origin_source,
    ) in rows
    {
        let position = MessagePosition {
            create_time: ts,
            local_id,
        };
        if status != 3 || origin_source != 2 {
            out.push(MessageEvent {
                room: username.to_string(),
                shard: shard.to_string(),
                position,
                message: None,
            });
            continue;
        }
        let content = decompress_message(&content_bytes, ct);
        let from = sender_username(real_sender_id, &content, is_group, username, &id2u);
        let text = fmt_content(local_id, local_type, &content, is_group);
        let quote = quote_for_message(local_type, &content, is_group);
        let msgtype = msgtype_for_message(local_type, &content, is_group, quote.is_some());
        let mut msg = json!({
            "msgid": server_id.to_string(),
            "local_id": local_id,
            "action": "send",
            "from": from,
            "tolist": [],
            "roomid": username,
            "msgtime": ts.saturating_mul(1000),
            "msgtype": msgtype,
        });
        if let Some(quote) = quote {
            msg["text"] = json!({ "content": quote_current_text(strip_group_prefix(&content, is_group)), "quote": quote_to_json(&quote) });
        } else if is_text_message(local_type) {
            msg["text"] = json!({ "content": text });
        } else if msgtype == "link" {
            msg["link"] = appmsg_link_body(strip_group_prefix(&content, is_group));
        } else if msgtype == "sphfeed" && base_type(local_type) == 49 {
            msg["sphfeed"] = appmsg_sphfeed_body(strip_group_prefix(&content, is_group))
                .unwrap_or_else(|| json!({ "content": text }));
        } else {
            msg[msgtype] = json!({ "content": text });
        }
        out.push(MessageEvent {
            room: username.to_string(),
            shard: shard.to_string(),
            position,
            message: Some(msg),
        });
    }
    Ok(out)
}

fn max_message_position(db_path: &Path, table: &str) -> Result<Option<MessagePosition>> {
    let conn = Connection::open(db_path)?;
    let sql = format!(
        "SELECT create_time, local_id FROM [{}] ORDER BY create_time DESC, local_id DESC LIMIT 1",
        table
    );
    conn.query_row(&sql, [], |row| {
        Ok(MessagePosition {
            create_time: row.get(0)?,
            local_id: row.get(1)?,
        })
    })
    .optional()
    .map_err(Into::into)
}

fn media_message_content(
    db_path: &Path,
    table: &str,
    server_id: i64,
    allowed_base_types: &[i64],
) -> Result<Option<(i64, String, i64, i64)>> {
    let conn = Connection::open(db_path)?;
    let sql = format!(
        "SELECT local_id, local_type, create_time, message_content, WCDB_CT_message_content
         FROM [{}] WHERE server_id = ? LIMIT 1",
        table
    );
    let row = conn
        .query_row(&sql, [server_id], |row| {
            Ok((
                row.get::<_, i64>(0)?,
                row.get::<_, i64>(1)?,
                row.get::<_, i64>(2)?,
                get_content_bytes(row, 3),
                row.get::<_, i64>(4).unwrap_or(0),
            ))
        })
        .optional()?;
    let Some((local_id, local_type, create_time, content_bytes, ct)) = row else {
        return Ok(None);
    };
    let local_type = base_type(local_type);
    if !allowed_base_types.contains(&local_type) {
        return Ok(None);
    }
    Ok(Some((
        local_id,
        decompress_message(&content_bytes, ct),
        create_time,
        local_type,
    )))
}

fn find_emotion_media(account_dir: &Path, content: &str) -> Result<Option<MediaFile>> {
    let Some(md5) = xml_attr(content, "md5").filter(|value| is_hex_like(value, 32)) else {
        return Ok(None);
    };

    find_local_emotion_media(account_dir, &md5)
}

fn find_local_emotion_media(account_dir: &Path, md5: &str) -> Result<Option<MediaFile>> {
    let root = account_dir.join("cache");
    if !root.is_dir() {
        return Ok(None);
    }

    let mut found = None;
    visit_files_sorted(&root, &mut |path| {
        if path.file_name().and_then(|name| name.to_str()) != Some(md5) {
            return Ok(false);
        }
        let data = fs::read(path)?;
        if let Some(media) = image_media_from_bytes(data, md5)? {
            found = Some(media);
            return Ok(true);
        }
        Ok(false)
    })?;
    Ok(found)
}

fn image_media_from_bytes(data: Vec<u8>, filename_stem: &str) -> Result<Option<MediaFile>> {
    let data = if data.starts_with(b"wxgf") {
        convert_wxgf_to_jpeg(&data)?
    } else {
        data
    };
    let content_type = detect_media_content_type(&data);
    if content_type == "application/octet-stream" {
        return Ok(None);
    }
    let ext = extension_for_content_type(&content_type);
    Ok(Some(MediaFile {
        data,
        content_type,
        filename: format!("{filename_stem}.{ext}"),
    }))
}

fn find_image_file(
    account_dir: &Path,
    local_id: i64,
    create_time: i64,
    content: &str,
) -> Result<Option<MediaFile>> {
    let mut filenames = vec![
        format!("{local_id}_{create_time}_thumb.jpg"),
        format!("{local_id}_{create_time}_t.dat"),
        format!("{local_id}_{create_time}_b.dat"),
    ];
    if let Some(md5) = xml_attr(content, "md5").filter(|value| is_hex_like(value, 32)) {
        filenames.extend([
            format!("{md5}.dat"),
            format!("{md5}_t.dat"),
            format!("{md5}_b.dat"),
            format!("{md5}_thumb.jpg"),
        ]);
    }

    let roots = [
        account_dir.join("msg").join("attach"),
        account_dir.join("cache"),
    ];
    for root in roots {
        for filename in &filenames {
            if let Some(media) = find_decodable_image_by_name(&root, filename)? {
                return Ok(Some(media));
            }
        }
    }
    Ok(None)
}

fn find_video_file(
    account_dir: &Path,
    local_id: i64,
    create_time: i64,
    content: &str,
) -> Result<Option<MediaFile>> {
    let mut stems = vec![local_id.to_string(), format!("{local_id}_{create_time}")];
    for attr in [
        "md5",
        "cdnvideomd5",
        "filemd5",
        "newmd5",
        "rawmd5",
        "thumbmd5",
    ] {
        if let Some(value) = xml_attr(content, attr).filter(|value| is_hex_like(value, 32)) {
            stems.push(value.to_ascii_lowercase());
        }
    }
    stems.sort();
    stems.dedup();

    let video_root = account_dir.join("msg").join("video");
    for dir in candidate_video_dirs(&video_root) {
        for stem in &stems {
            for filename in video_candidate_filenames(stem) {
                let path = dir.join(&filename);
                if let Some(media) = video_media_from_file(&path, stem)? {
                    return Ok(Some(media));
                }
            }
        }
    }
    Ok(None)
}

fn candidate_video_dirs(video_root: &Path) -> Vec<PathBuf> {
    let mut dirs = vec![video_root.to_path_buf()];
    if let Ok(entries) = fs::read_dir(video_root) {
        for entry in entries.flatten() {
            let path = entry.path();
            if path.is_dir() {
                dirs.push(path);
            }
        }
    }
    dirs
}

fn video_candidate_filenames(stem: &str) -> Vec<String> {
    vec![
        format!("{stem}.mp4"),
        format!("{stem}.mp4.dat"),
        format!("{stem}.dat"),
        stem.to_string(),
    ]
}

fn video_media_from_file(path: &Path, filename_stem: &str) -> Result<Option<MediaFile>> {
    if !path.is_file() {
        return Ok(None);
    }
    let metadata = fs::metadata(path)?;
    if metadata.len() == 0 || metadata.len() > MAX_LOCAL_MEDIA_BYTES {
        return Ok(None);
    }
    let data = fs::read(path)?;
    video_media_from_bytes(data, filename_stem)
}

fn video_media_from_bytes(data: Vec<u8>, filename_stem: &str) -> Result<Option<MediaFile>> {
    let content_type = detect_media_content_type(&data);
    if !content_type.starts_with("video/") {
        return Ok(None);
    }
    let ext = extension_for_content_type(&content_type);
    Ok(Some(MediaFile {
        data,
        content_type,
        filename: format!("{filename_stem}.{ext}"),
    }))
}

fn find_resource_image_file(
    db: &mut DbCache,
    account_dir: &Path,
    attach_root: &Path,
    roomid: &str,
    local_id: i64,
    create_time: i64,
    local_type: i64,
) -> Result<Option<MediaFile>> {
    let Some(resource_db) = db.get("message/message_resource.db")? else {
        return Ok(None);
    };

    let mut candidate_types = vec![local_type];
    if local_type != 3 {
        candidate_types.push(3);
    }

    for msg_type in candidate_types {
        let Some(file_md5) =
            lookup_resource_md5(&resource_db, roomid, local_id, create_time, msg_type)?
        else {
            continue;
        };
        let Some(dat_path) = find_dat_file(attach_root, roomid, &file_md5) else {
            continue;
        };
        let media = decode_dat_image(account_dir, attach_root, &dat_path, &file_md5)
            .with_context(|| format!("解码本地图片失败: {}", dat_path.display()))?;
        return Ok(Some(media));
    }
    Ok(None)
}

fn lookup_resource_md5(
    resource_db_path: &Path,
    chat: &str,
    local_id: i64,
    create_time: i64,
    msg_local_type_lo32: i64,
) -> Result<Option<String>> {
    let conn = Connection::open(resource_db_path).with_context(|| {
        format!(
            "打开 message_resource.db 失败: {}",
            resource_db_path.display()
        )
    })?;
    let chat_id: Option<i64> = conn
        .query_row(
            "SELECT rowid FROM ChatName2Id WHERE user_name = ?1",
            [chat],
            |row| row.get(0),
        )
        .optional()?;
    let Some(chat_id) = chat_id else {
        return Ok(None);
    };

    let packed_exact: Option<Vec<u8>> = conn
        .query_row(
            "SELECT packed_info FROM MessageResourceInfo
             WHERE chat_id = ?1
               AND message_local_id = ?2
               AND (message_local_type = ?3 OR message_local_type % 4294967296 = ?3)
               AND message_create_time = ?4
             ORDER BY rowid DESC
             LIMIT 1",
            rusqlite::params![chat_id, local_id, msg_local_type_lo32, create_time],
            |row| row.get(0),
        )
        .optional()?;

    let packed = match packed_exact {
        Some(blob) => Some(blob),
        None => conn
            .query_row(
                "SELECT packed_info FROM MessageResourceInfo
                 WHERE chat_id = ?1
                   AND message_local_id = ?2
                   AND (message_local_type = ?3 OR message_local_type % 4294967296 = ?3)
                 ORDER BY message_create_time DESC
                 LIMIT 1",
                rusqlite::params![chat_id, local_id, msg_local_type_lo32],
                |row| row.get(0),
            )
            .optional()?,
    };

    let Some(blob) = packed else {
        return Ok(None);
    };
    Ok(extract_resource_md5_from_packed_info(&blob))
}

fn extract_resource_md5_from_packed_info(blob: &[u8]) -> Option<String> {
    const MARKER: &[u8; 4] = &[0x12, 0x22, 0x0A, 0x20];

    if let Some(pos) = find_subslice(blob, MARKER) {
        let start = pos + MARKER.len();
        if start + 32 <= blob.len() {
            if let Ok(value) = std::str::from_utf8(&blob[start..start + 32]) {
                if is_hex_like(value, 32) {
                    return Some(value.to_ascii_lowercase());
                }
            }
        }
    }

    if blob.len() >= 32 {
        for start in 0..=blob.len() - 32 {
            let chunk = &blob[start..start + 32];
            if let Ok(value) = std::str::from_utf8(chunk) {
                if is_hex_like(value, 32) {
                    return Some(value.to_ascii_lowercase());
                }
            }
        }
    }
    None
}

fn find_subslice(haystack: &[u8], needle: &[u8]) -> Option<usize> {
    if needle.is_empty() || needle.len() > haystack.len() {
        return None;
    }
    haystack
        .windows(needle.len())
        .position(|window| window == needle)
}

fn find_dat_file(attach_root: &Path, chat: &str, file_md5: &str) -> Option<PathBuf> {
    let chat_hash = format!("{:x}", md5::compute(chat.as_bytes()));
    let chat_dir = attach_root.join(chat_hash);
    if !chat_dir.is_dir() {
        return None;
    }

    let mut months: Vec<PathBuf> = fs::read_dir(&chat_dir)
        .ok()?
        .flatten()
        .map(|entry| entry.path())
        .filter(|path| path.is_dir())
        .collect();
    months.sort();
    for month_dir in months {
        if let Some(path) = pick_best_in_img_dir(&month_dir.join("Img"), file_md5) {
            return Some(path);
        }
    }
    None
}

fn pick_best_in_img_dir(img_dir: &Path, file_md5: &str) -> Option<PathBuf> {
    if !img_dir.is_dir() {
        return None;
    }
    for suffix in [".dat", "_h.dat", "_t.dat"] {
        let path = img_dir.join(format!("{file_md5}{suffix}"));
        if path.is_file() {
            return Some(path);
        }
    }
    None
}

fn decode_dat_image(
    account_dir: &Path,
    attach_root: &Path,
    path: &Path,
    file_md5: &str,
) -> Result<MediaFile> {
    let data = fs::read(path)?;
    let direct_content_type = detect_media_content_type(&data);
    let data = if direct_content_type == "application/octet-stream" {
        if data.starts_with(&V2_IMAGE_MAGIC) {
            let key = image_key_for_account(account_dir, attach_root)?;
            decode_v2_image(&data, &key.aes_key, key.xor_key)?
        } else if data.starts_with(&V1_IMAGE_MAGIC) {
            let fixed_key: [u8; 16] = *b"cfcd208495d565ef";
            decode_v2_image(&data, &fixed_key, 0x88)?
        } else {
            decode_legacy_xor_image(&data)?
        }
    } else {
        data
    };

    let data = if data.starts_with(b"wxgf") {
        convert_wxgf_to_jpeg(&data)?
    } else {
        data
    };

    let content_type = detect_media_content_type(&data);
    if content_type == "application/octet-stream" {
        bail!("图片解码产物不是可识别图片格式");
    }
    let ext = extension_for_content_type(&content_type);
    Ok(MediaFile {
        data,
        content_type,
        filename: format!("{file_md5}.{ext}"),
    })
}

fn convert_wxgf_to_jpeg(data: &[u8]) -> Result<Vec<u8>> {
    let Some(hevc_payload) = wxgf_hevc_payload(data) else {
        bail!("wxgf 缺少 HEVC payload");
    };

    let tmp = std::env::temp_dir();
    let id = uuid::Uuid::new_v4();
    let input_path = tmp.join(format!("webox-wxgf-{id}.h265"));
    let output_path = tmp.join(format!("webox-wxgf-{id}.jpg"));
    fs::write(&input_path, hevc_payload)
        .with_context(|| format!("写入 wxgf 临时输入失败: {}", input_path.display()))?;

    let output = Command::new("ffmpeg")
        .args([
            "-hide_banner",
            "-loglevel",
            "error",
            "-y",
            "-f",
            "hevc",
            "-i",
            input_path.to_string_lossy().as_ref(),
            "-frames:v",
            "1",
            "-q:v",
            "2",
            output_path.to_string_lossy().as_ref(),
        ])
        .output();

    let _ = fs::remove_file(&input_path);
    let output = output.context("启动 ffmpeg 转换 wxgf 失败")?;
    if !output.status.success() {
        let _ = fs::remove_file(&output_path);
        let stderr = String::from_utf8_lossy(&output.stderr);
        bail!("ffmpeg 转换 wxgf 失败: {}", stderr.trim());
    }

    let jpeg = fs::read(&output_path)
        .with_context(|| format!("读取 ffmpeg JPEG 输出失败: {}", output_path.display()));
    let _ = fs::remove_file(&output_path);
    let jpeg = jpeg?;
    if detect_media_content_type(&jpeg) != "image/jpeg" {
        bail!("ffmpeg 转换 wxgf 后未生成 JPEG");
    }
    Ok(jpeg)
}

fn wxgf_hevc_payload(data: &[u8]) -> Option<&[u8]> {
    if !data.starts_with(b"wxgf") {
        return None;
    }
    let start = find_subslice(data, b"\x00\x00\x00\x01")?;
    Some(&data[start..])
}

fn decode_v2_image(file_bytes: &[u8], aes_key: &[u8; 16], xor_key: u8) -> Result<Vec<u8>> {
    if file_bytes.len() < V2_IMAGE_HEADER_SIZE {
        bail!("V2 图片文件过短");
    }
    let magic: &[u8; 6] = file_bytes[..6].try_into().unwrap();
    if magic != &V2_IMAGE_MAGIC && magic != &V1_IMAGE_MAGIC {
        bail!("V2 图片 header magic 不匹配");
    }

    let aes_size = u32::from_le_bytes(file_bytes[6..10].try_into().unwrap()) as usize;
    let xor_size = u32::from_le_bytes(file_bytes[10..14].try_into().unwrap()) as usize;
    let aligned_aes_size = aes_size + (16 - (aes_size % 16));
    let aes_end = V2_IMAGE_HEADER_SIZE
        .checked_add(aligned_aes_size)
        .ok_or_else(|| anyhow!("V2 图片 AES 段长度溢出"))?;
    if aes_end > file_bytes.len() {
        bail!("V2 图片 AES 段超过文件长度");
    }
    let raw_end = file_bytes
        .len()
        .checked_sub(xor_size)
        .ok_or_else(|| anyhow!("V2 图片 XOR 段超过文件长度"))?;
    if aes_end > raw_end {
        bail!("V2 图片 AES/XOR 段重叠");
    }

    let aes_data = aes_ecb_decrypt_pkcs7(aes_key, &file_bytes[V2_IMAGE_HEADER_SIZE..aes_end])?;
    let raw_data = &file_bytes[aes_end..raw_end];
    let xor_data = file_bytes[raw_end..]
        .iter()
        .map(|byte| byte ^ xor_key)
        .collect::<Vec<_>>();

    let mut out = Vec::with_capacity(aes_data.len() + raw_data.len() + xor_data.len());
    out.extend_from_slice(&aes_data);
    out.extend_from_slice(raw_data);
    out.extend_from_slice(&xor_data);
    Ok(out)
}

fn aes_ecb_decrypt_pkcs7(key: &[u8; 16], cipher: &[u8]) -> Result<Vec<u8>> {
    if cipher.is_empty() || !cipher.len().is_multiple_of(16) {
        bail!("AES 输入长度不是 16 的倍数");
    }
    let aes = Aes128::new(key.into());
    let mut out = Vec::with_capacity(cipher.len());
    for chunk in cipher.chunks_exact(16) {
        let mut block = GenericArray::clone_from_slice(chunk);
        aes.decrypt_block(&mut block);
        out.extend_from_slice(&block);
    }
    let pad = *out.last().ok_or_else(|| anyhow!("AES 解密输出为空"))? as usize;
    if pad == 0 || pad > 16 || pad > out.len() {
        bail!("AES PKCS7 padding 长度非法");
    }
    if !out[out.len() - pad..]
        .iter()
        .all(|byte| *byte as usize == pad)
    {
        bail!("AES PKCS7 padding 字节非法");
    }
    out.truncate(out.len() - pad);
    Ok(out)
}

fn decode_legacy_xor_image(file_bytes: &[u8]) -> Result<Vec<u8>> {
    let key =
        detect_legacy_xor_key(file_bytes).ok_or_else(|| anyhow!("无法识别 legacy XOR 图片"))?;
    Ok(file_bytes.iter().map(|byte| byte ^ key).collect())
}

fn detect_legacy_xor_key(file_bytes: &[u8]) -> Option<u8> {
    if file_bytes.len() < 4 {
        return None;
    }
    let header = &file_bytes[..file_bytes.len().min(16)];
    for magic in [
        b"\x89PNG".as_slice(),
        b"GIF8".as_slice(),
        &[0x49, 0x49, 0x2A, 0x00],
        b"RIFF".as_slice(),
        &[0xFF, 0xD8, 0xFF],
    ] {
        if let Some(key) = detect_xor_key_for_magic(header, magic) {
            return Some(key);
        }
    }
    detect_bmp_xor_key(file_bytes, header)
}

fn detect_xor_key_for_magic(header: &[u8], magic: &[u8]) -> Option<u8> {
    if header.len() < magic.len() {
        return None;
    }
    let key = header[0] ^ magic[0];
    magic
        .iter()
        .enumerate()
        .all(|(idx, expected)| header[idx] ^ key == *expected)
        .then_some(key)
}

fn detect_bmp_xor_key(file_bytes: &[u8], header: &[u8]) -> Option<u8> {
    let key = detect_xor_key_for_magic(header, b"BM")?;
    if header.len() < 14 {
        return None;
    }
    let mut decoded = [0u8; 14];
    for idx in 0..14 {
        decoded[idx] = header[idx] ^ key;
    }
    let bmp_size = u32::from_le_bytes([decoded[2], decoded[3], decoded[4], decoded[5]]);
    let bmp_offset = u32::from_le_bytes([decoded[10], decoded[11], decoded[12], decoded[13]]);
    let file_size = file_bytes.len() as u32;
    (file_size.abs_diff(bmp_size) < 1024 && (14..=1078).contains(&bmp_offset)).then_some(key)
}

fn image_key_for_account(account_dir: &Path, attach_root: &Path) -> Result<ImageKeyMaterial> {
    static IMAGE_KEY_CACHE: OnceLock<Mutex<HashMap<String, ImageKeyMaterial>>> = OnceLock::new();

    let cache_key = account_dir.to_string_lossy().into_owned();
    let cache = IMAGE_KEY_CACHE.get_or_init(|| Mutex::new(HashMap::new()));
    if let Some(found) = cache
        .lock()
        .map_err(|_| anyhow!("image key cache lock is poisoned"))?
        .get(&cache_key)
        .copied()
    {
        return Ok(found);
    }

    let key = derive_image_key_for_account(account_dir, attach_root)?;
    cache
        .lock()
        .map_err(|_| anyhow!("image key cache lock is poisoned"))?
        .insert(cache_key, key);
    Ok(key)
}

fn derive_image_key_for_account(
    account_dir: &Path,
    attach_root: &Path,
) -> Result<ImageKeyMaterial> {
    let templates = find_v2_template_ciphertexts(attach_root, 3, 64)?;
    if templates.is_empty() {
        bail!("找不到 V2 图片模板，无法派生本地图片 key");
    }

    let (wxid_raw, wxid_normalized, suffix) = account_wxid_parts(account_dir)
        .ok_or_else(|| anyhow!("账号目录缺少 wxid 4 位后缀，无法派生本地图片 key"))?;
    let xor_key = derive_xor_key_from_v2_dat(attach_root, 10, 1)?
        .ok_or_else(|| anyhow!("V2 图片样本不足，无法派生 XOR key"))?;

    for wxid in preferred_wxid_candidates(&wxid_raw, &wxid_normalized) {
        if let Some(aes_key) = bruteforce_image_aes_key(xor_key, &suffix, wxid, &templates)? {
            return Ok(ImageKeyMaterial { aes_key, xor_key });
        }
    }

    bail!("派生本地图片 key 失败");
}

fn account_wxid_parts(account_dir: &Path) -> Option<(String, String, String)> {
    let raw = account_dir.file_name()?.to_string_lossy().into_owned();
    let idx = raw.rfind('_')?;
    let suffix = &raw[idx + 1..];
    if suffix.len() != 4 || !suffix.bytes().all(|byte| byte.is_ascii_hexdigit()) {
        return None;
    }
    Some((
        raw.clone(),
        normalize_wxid(&raw),
        suffix.to_ascii_lowercase(),
    ))
}

fn normalize_wxid(raw: &str) -> String {
    let raw = raw.trim();
    if let Some(stripped) = raw.strip_prefix("wxid_") {
        let head = stripped.split('_').next().unwrap_or(stripped);
        return format!("wxid_{head}");
    }
    if let Some((base, suffix)) = raw.rsplit_once('_') {
        if suffix.len() == 4 && suffix.bytes().all(|byte| byte.is_ascii_hexdigit()) {
            return base.to_string();
        }
    }
    raw.to_string()
}

fn preferred_wxid_candidates<'a>(raw: &'a str, normalized: &'a str) -> Vec<&'a str> {
    if raw == normalized {
        vec![raw]
    } else {
        vec![normalized, raw]
    }
}

fn find_v2_template_ciphertexts(
    attach_root: &Path,
    max_templates: usize,
    max_files: usize,
) -> Result<Vec<[u8; 16]>> {
    let mut out =
        collect_v2_templates_with_suffix(attach_root, "_t.dat", max_templates, max_files)?;
    if out.is_empty() {
        out = collect_v2_templates_with_suffix(attach_root, ".dat", max_templates, max_files)?;
    }
    Ok(out)
}

fn collect_v2_templates_with_suffix(
    attach_root: &Path,
    suffix: &str,
    max_templates: usize,
    max_files: usize,
) -> Result<Vec<[u8; 16]>> {
    if !attach_root.is_dir() {
        return Ok(Vec::new());
    }
    let mut out = Vec::new();
    let mut seen = HashSet::new();
    let mut examined = 0usize;
    visit_files_sorted(attach_root, &mut |path| {
        let Some(name) = path.file_name().and_then(|value| value.to_str()) else {
            return Ok(false);
        };
        if !name.ends_with(suffix) {
            return Ok(false);
        }
        examined += 1;
        let bytes = fs::read(path)?;
        if bytes.len() >= V2_IMAGE_HEADER_SIZE + 16 && bytes.starts_with(&V2_IMAGE_MAGIC) {
            let mut block = [0u8; 16];
            block.copy_from_slice(&bytes[V2_IMAGE_HEADER_SIZE..V2_IMAGE_HEADER_SIZE + 16]);
            if seen.insert(block) {
                out.push(block);
            }
        }
        Ok(out.len() >= max_templates || examined >= max_files)
    })?;
    Ok(out)
}

fn derive_xor_key_from_v2_dat(
    attach_root: &Path,
    sample: usize,
    min_samples: usize,
) -> Result<Option<u8>> {
    if !attach_root.is_dir() {
        return Ok(None);
    }
    let mut votes = Vec::new();
    visit_files_sorted(attach_root, &mut |path| {
        let Some(name) = path.file_name().and_then(|value| value.to_str()) else {
            return Ok(false);
        };
        if !name.ends_with(".dat") {
            return Ok(false);
        }
        let bytes = fs::read(path)?;
        if bytes.len() >= V2_IMAGE_HEADER_SIZE + 16 && bytes.starts_with(&V2_IMAGE_MAGIC) {
            if let Some(last) = bytes.last() {
                votes.push(last ^ 0xD9);
            }
        }
        Ok(votes.len() >= sample)
    })?;

    if votes.len() < min_samples {
        return Ok(None);
    }
    let mut counts = [0usize; 256];
    for vote in votes {
        counts[vote as usize] += 1;
    }
    Ok(counts
        .iter()
        .enumerate()
        .max_by_key(|(_, count)| *count)
        .map(|(idx, _)| idx as u8))
}

fn bruteforce_image_aes_key(
    xor_key: u8,
    suffix_hex: &str,
    wxid: &str,
    templates: &[[u8; 16]],
) -> Result<Option<[u8; 16]>> {
    let suffix = hex_prefix_to_bytes(suffix_hex)?;
    let workers = std::thread::available_parallelism()
        .map(|count| count.get())
        .unwrap_or(1)
        .clamp(1, 64);
    let total = 1u32 << 24;
    let chunk = total / workers as u32;
    let stop = Arc::new(AtomicBool::new(false));
    let wxid = Arc::new(wxid.as_bytes().to_vec());
    let templates = Arc::new(templates.to_vec());
    let (tx, rx) = mpsc::channel();

    std::thread::scope(|scope| {
        for idx in 0..workers {
            let start = idx as u32 * chunk;
            let end = if idx + 1 == workers {
                total
            } else {
                (idx as u32 + 1) * chunk
            };
            let stop = Arc::clone(&stop);
            let wxid = Arc::clone(&wxid);
            let templates = Arc::clone(&templates);
            let tx = tx.clone();
            scope.spawn(move || {
                for upper in start..end {
                    if stop.load(Ordering::Relaxed) {
                        break;
                    }
                    let uin = (upper << 8) | xor_key as u32;
                    let uin_ascii = uin.to_string();
                    let digest = md5::compute(uin_ascii.as_bytes());
                    if digest.0[0] != suffix[0] || digest.0[1] != suffix[1] {
                        continue;
                    }

                    let mut input = Vec::with_capacity(uin_ascii.len() + wxid.len());
                    input.extend_from_slice(uin_ascii.as_bytes());
                    input.extend_from_slice(&wxid);
                    let aes_hex = format!("{:x}", md5::compute(input));
                    let mut aes_key = [0u8; 16];
                    aes_key.copy_from_slice(&aes_hex.as_bytes()[..16]);
                    if verify_image_aes_key(&aes_key, &templates) {
                        stop.store(true, Ordering::Relaxed);
                        let _ = tx.send(aes_key);
                        break;
                    }
                }
            });
        }
    });
    drop(tx);
    Ok(rx.try_iter().next())
}

fn hex_prefix_to_bytes(hex: &str) -> Result<[u8; 2]> {
    if hex.len() != 4 {
        bail!("wxid suffix 不是 4 位 hex");
    }
    Ok([
        u8::from_str_radix(&hex[..2], 16)?,
        u8::from_str_radix(&hex[2..], 16)?,
    ])
}

fn verify_image_aes_key(aes_key: &[u8; 16], templates: &[[u8; 16]]) -> bool {
    !templates.is_empty()
        && templates
            .iter()
            .all(|template| decrypt_template_block(aes_key, template).is_some())
}

fn decrypt_template_block(aes_key: &[u8; 16], template: &[u8; 16]) -> Option<[u8; 16]> {
    let aes = Aes128::new(aes_key.into());
    let mut block = GenericArray::clone_from_slice(template);
    aes.decrypt_block(&mut block);
    let mut out = [0u8; 16];
    out.copy_from_slice(&block);
    (detect_media_content_type(&out) != "application/octet-stream").then_some(out)
}

fn visit_files_sorted<F>(dir: &Path, visit: &mut F) -> Result<bool>
where
    F: FnMut(&Path) -> Result<bool>,
{
    let mut entries: Vec<_> = fs::read_dir(dir)?.flatten().collect();
    entries.sort_by_key(|entry| entry.path());
    for entry in entries {
        let path = entry.path();
        if path.is_dir() {
            if visit_files_sorted(&path, visit)? {
                return Ok(true);
            }
        } else if visit(&path)? {
            return Ok(true);
        }
    }
    Ok(false)
}

fn find_decodable_image_by_name(root: &Path, filename: &str) -> Result<Option<MediaFile>> {
    let Some(path) = find_file_by_name(root, filename)? else {
        return Ok(None);
    };
    let data = fs::read(&path)?;
    let content_type = detect_media_content_type(&data);
    if content_type == "application/octet-stream" {
        return Ok(None);
    }
    Ok(Some(MediaFile {
        data,
        content_type,
        filename: filename.to_string(),
    }))
}

fn find_file_by_name(root: &Path, filename: &str) -> Result<Option<PathBuf>> {
    if !root.is_dir() {
        return Ok(None);
    }
    let mut stack = vec![root.to_path_buf()];
    while let Some(dir) = stack.pop() {
        for entry in fs::read_dir(&dir)? {
            let entry = entry?;
            let path = entry.path();
            if path.is_dir() {
                stack.push(path);
                continue;
            }
            if entry.file_name().to_string_lossy() == filename {
                return Ok(Some(path));
            }
        }
    }
    Ok(None)
}

fn detect_media_content_type(data: &[u8]) -> String {
    if data.starts_with(b"wxgf") {
        return "image/heic".to_string();
    }
    if data.starts_with(&[0xFF, 0xD8, 0xFF]) {
        return "image/jpeg".to_string();
    }
    if data.starts_with(b"\x89PNG\r\n\x1a\n") {
        return "image/png".to_string();
    }
    if data.starts_with(b"GIF8") {
        return "image/gif".to_string();
    }
    if data.starts_with(b"RIFF") && data.get(8..12) == Some(b"WEBP") {
        return "image/webp".to_string();
    }
    if data.len() >= 12 && data.get(4..8) == Some(b"ftyp") {
        return "video/mp4".to_string();
    }
    if data.starts_with(b"BM") {
        return "image/bmp".to_string();
    }
    if data.starts_with(&[0x49, 0x49, 0x2A, 0x00]) {
        return "image/tiff".to_string();
    }
    "application/octet-stream".to_string()
}

fn extension_for_content_type(content_type: &str) -> &'static str {
    match content_type {
        "image/heic" => "heic",
        "image/jpeg" => "jpg",
        "image/png" => "png",
        "image/gif" => "gif",
        "image/webp" => "webp",
        "image/bmp" => "bmp",
        "image/tiff" => "tif",
        "video/mp4" => "mp4",
        _ => "bin",
    }
}

fn xml_attr(xml: &str, name: &str) -> Option<String> {
    let name_len = name.len();
    let mut offset = 0;
    while let Some(pos_rel) = xml[offset..].find(name) {
        let pos = offset + pos_rel;
        let before_is_name = pos > 0
            && xml
                .as_bytes()
                .get(pos - 1)
                .is_some_and(|byte| is_xml_attr_name_byte(*byte));
        if before_is_name {
            offset = pos + name_len;
            continue;
        }

        let mut i = pos + name_len;
        while xml
            .as_bytes()
            .get(i)
            .is_some_and(|byte| byte.is_ascii_whitespace())
        {
            i += 1;
        }
        if xml.as_bytes().get(i) != Some(&b'=') {
            offset = pos + name_len;
            continue;
        }
        i += 1;
        while xml
            .as_bytes()
            .get(i)
            .is_some_and(|byte| byte.is_ascii_whitespace())
        {
            i += 1;
        }
        let quote = *xml.as_bytes().get(i)?;
        if quote != b'"' && quote != b'\'' {
            offset = pos + name_len;
            continue;
        }
        let value_start = i + 1;
        let end_rel = xml.as_bytes()[value_start..]
            .iter()
            .position(|byte| *byte == quote)?;
        return Some(unescape_html(&xml[value_start..value_start + end_rel]));
    }
    None
}

fn is_xml_attr_name_byte(byte: u8) -> bool {
    byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-' | b':')
}

fn is_http_url(value: &str) -> bool {
    let value = value.trim();
    value.len() <= 4096 && (value.starts_with("http://") || value.starts_with("https://"))
}

fn is_hex_like(value: &str, len: usize) -> bool {
    value.len() == len && value.as_bytes().iter().all(u8::is_ascii_hexdigit)
}

fn load_id2u(conn: &Connection) -> HashMap<i64, String> {
    let mut map = HashMap::new();
    if let Ok(mut stmt) = conn.prepare("SELECT rowid, user_name FROM Name2Id") {
        let _ = stmt
            .query_map([], |row| {
                Ok((row.get::<_, i64>(0)?, row.get::<_, String>(1)?))
            })
            .map(|rows| {
                for row in rows.flatten() {
                    map.insert(row.0, row.1);
                }
            });
    }
    map
}

fn sender_username(
    real_sender_id: i64,
    content: &str,
    is_group: bool,
    chat_username: &str,
    id2u: &HashMap<i64, String>,
) -> String {
    let sender_uname = id2u.get(&real_sender_id).cloned().unwrap_or_default();
    if is_group {
        if !sender_uname.is_empty() && sender_uname != chat_username {
            return sender_uname;
        }
        if content.contains(":\n") {
            return content.split(":\n").next().unwrap_or("").to_string();
        }
        return String::new();
    }
    if !sender_uname.is_empty() && sender_uname != chat_username {
        return sender_uname;
    }
    chat_username.to_string()
}

fn get_content_bytes(row: &rusqlite::Row<'_>, idx: usize) -> Vec<u8> {
    row.get::<_, Vec<u8>>(idx)
        .or_else(|_| row.get::<_, String>(idx).map(|s| s.into_bytes()))
        .unwrap_or_default()
}

fn decompress_message(data: &[u8], ct: i64) -> String {
    if ct == 4 && !data.is_empty() {
        if let Ok(dec) = zstd::decode_all(data) {
            return String::from_utf8_lossy(&dec).into_owned();
        }
    }
    String::from_utf8_lossy(data).into_owned()
}

fn base_type(t: i64) -> i64 {
    (t as u64 & 0xFFFFFFFF) as i64
}

fn msgtype_for_message(t: i64, content: &str, is_group: bool, is_quote: bool) -> &'static str {
    if is_quote {
        return "text";
    }
    if base_type(t) == 49 {
        let content = strip_group_prefix(content, is_group);
        if appmsg_sphfeed_body(content).is_some() {
            return "sphfeed";
        }
        if appmsg_type(content) == Some(6) {
            return "file";
        }
    }
    msgtype_for_base(base_type(t))
}

fn msgtype_for_base(base: i64) -> &'static str {
    match base {
        1 => "text",
        3 => "image",
        34 => "voice",
        42 => "card",
        43 => "video",
        47 => "emotion",
        48 => "location",
        49 => "link",
        50 => "voip",
        10000 => "system",
        10002 => "revoke",
        _ => "unknown",
    }
}

fn fmt_content(_local_id: i64, local_type: i64, content: &str, is_group: bool) -> String {
    let base = base_type(local_type);
    match base {
        3 => return "[图片]".into(),
        34 => return "[语音]".into(),
        43 => return "[视频]".into(),
        47 => return "[表情]".into(),
        50 => return "[通话]".into(),
        10000 => return parse_sysmsg(content).unwrap_or_else(|| "[系统消息]".into()),
        10002 => return parse_revoke(content).unwrap_or_else(|| "[撤回了一条消息]".into()),
        _ => {}
    }
    let text = strip_group_prefix(content, is_group);
    if base == 49 && text.contains("<appmsg") {
        if let Some(parsed) = parse_appmsg(text) {
            return parsed;
        }
    }
    text.to_string()
}

fn strip_group_prefix(content: &str, is_group: bool) -> &str {
    if is_group && content.contains(":\n") {
        content
            .split_once(":\n")
            .map(|(_, value)| value)
            .unwrap_or(content)
    } else {
        content
    }
}

fn parse_revoke(xml: &str) -> Option<String> {
    let inner = extract_xml_text(xml, "content")?;
    Some(if inner.is_empty() {
        "[撤回了一条消息]".into()
    } else {
        format!("[撤回] {}", inner.chars().take(30).collect::<String>())
    })
}

fn parse_sysmsg(xml: &str) -> Option<String> {
    if let Some(s) = extract_xml_text(xml, "content") {
        if !s.is_empty() {
            return Some(format!("[系统] {}", s.chars().take(50).collect::<String>()));
        }
    }
    if !xml.starts_with('<') {
        return Some(format!(
            "[系统] {}",
            xml.chars().take(50).collect::<String>()
        ));
    }
    Some("[系统消息]".into())
}

fn parse_appmsg(text: &str) -> Option<String> {
    if parse_refermsg(text).is_some() {
        return Some(quote_current_text(text));
    }
    if let Some(body) = appmsg_sphfeed_body(text) {
        return Some(sphfeed_display_text(&body));
    }
    let title = appmsg_title(text);
    let app_name = extract_xml_text(text, "appname").unwrap_or_default();
    if title.is_empty() && app_name.is_empty() {
        None
    } else if app_name.is_empty() {
        Some(format!("[链接] {title}"))
    } else if title.is_empty() {
        Some(format!("[链接] {app_name}"))
    } else {
        Some(format!("[链接] {title} - {app_name}"))
    }
}

fn appmsg_title(text: &str) -> String {
    clean_xml_text(extract_xml_text(text, "title").unwrap_or_default())
}

fn appmsg_type(text: &str) -> Option<i64> {
    extract_xml_text(text, "type")?.trim().parse().ok()
}

fn quote_for_message(local_type: i64, content: &str, is_group: bool) -> Option<ReferMsg> {
    if base_type(local_type) != 49 {
        return None;
    }
    parse_refermsg(strip_group_prefix(content, is_group))
}

fn parse_refermsg(text: &str) -> Option<ReferMsg> {
    let refer_xml = extract_xml_text(text, "refermsg")?;
    let raw_content = extract_xml_text(&refer_xml, "content").unwrap_or_default();
    let (msgtype, body, content) = format_refermsg_content(
        &raw_content,
        extract_xml_text(&refer_xml, "type").as_deref(),
    );
    Some(ReferMsg {
        chatusr: clean_xml_text(extract_xml_text(&refer_xml, "chatusr").unwrap_or_default()),
        displayname: clean_xml_text(
            extract_xml_text(&refer_xml, "displayname").unwrap_or_default(),
        ),
        msgid: clean_xml_text(extract_xml_text(&refer_xml, "svrid").unwrap_or_default()),
        msgtype,
        content,
        body,
    })
}

fn format_refermsg_content(raw: &str, refer_type: Option<&str>) -> (String, Value, String) {
    let content = clean_xml_text(raw.to_string());
    if content.contains("<img") {
        return typed_quote_body("image", "[图片]");
    }
    if content.contains("<videomsg") {
        return typed_quote_body("video", "[视频]");
    }
    if content.contains("<emoji") {
        return typed_quote_body("emotion", "[表情]");
    }
    if content.contains("<location") {
        return typed_quote_body("location", "[位置]");
    }
    if !looks_like_xml(&content) {
        return text_quote_body(content);
    }
    if content.contains("<appmsg") {
        if let Some(body) = appmsg_sphfeed_body(&content) {
            let display = sphfeed_display_text(&body);
            return ("sphfeed".to_string(), body, display);
        }
        let title = appmsg_title(&content);
        if extract_xml_text(&content, "type").as_deref() == Some("57") && !title.is_empty() {
            return text_quote_body(title);
        }
        let link = appmsg_link_body(&content);
        if !link["title"].as_str().unwrap_or("").is_empty()
            || !link["link_url"].as_str().unwrap_or("").is_empty()
        {
            let display = if !link["title"].as_str().unwrap_or("").is_empty() {
                link["title"].as_str().unwrap_or("").to_string()
            } else {
                "[链接]".to_string()
            };
            return ("link".to_string(), link, display);
        }
        if !title.is_empty() {
            return text_quote_body(title);
        }
    }
    match refer_type {
        Some("3") => typed_quote_body("image", "[图片]"),
        Some("43") => typed_quote_body("video", "[视频]"),
        Some("47") => typed_quote_body("emotion", "[表情]"),
        Some("49") => typed_quote_body("link", "[链接]"),
        _ => text_quote_body("[消息]".to_string()),
    }
}

fn text_quote_body(content: String) -> (String, Value, String) {
    let display = content.clone();
    ("text".to_string(), json!({ "content": content }), display)
}

fn typed_quote_body(msgtype: &str, content: &str) -> (String, Value, String) {
    (
        msgtype.to_string(),
        json!({ "content": content }),
        content.to_string(),
    )
}

fn looks_like_xml(text: &str) -> bool {
    let trimmed = text.trim_start();
    trimmed.starts_with('<') || trimmed.starts_with("<?xml")
}

fn quote_to_json(quote: &ReferMsg) -> Value {
    let mut out = json!({
        "msgtype": quote.msgtype,
    });
    let quoted_sender = if quote.displayname.is_empty() {
        quote.chatusr.as_str()
    } else {
        quote.displayname.as_str()
    };
    if !quoted_sender.is_empty() {
        out["from"] = json!(quoted_sender);
    }
    if !quote.msgid.is_empty() {
        out["msgid"] = json!(quote.msgid);
    }
    out[&quote.msgtype] = quote.body.clone();
    out
}

fn quote_current_text(text: &str) -> String {
    appmsg_title(text)
}

fn appmsg_link_body(text: &str) -> Value {
    json!({
        "title": appmsg_title(text),
        "description": clean_xml_text(extract_xml_text(text, "des").unwrap_or_default()),
        "link_url": appmsg_url_for_content(text).unwrap_or_default(),
    })
}

fn appmsg_sphfeed_body(text: &str) -> Option<Value> {
    let feed = extract_xml_text(text, "finderFeed")?;
    let media = extract_xml_text(&feed, "media").unwrap_or_default();
    let feed_type = xml_i64(&feed, "feedType")
        .or_else(|| xml_i64(&media, "mediaType"))
        .unwrap_or(4);
    let nickname = xml_clean_text(&feed, "nickname");
    let desc = xml_clean_text(&feed, "desc");
    let mut body = json!({
        "feed_type": feed_type,
        "sph_name": nickname,
        "feed_desc": desc,
    });
    let url = xml_clean_text(&media, "url");
    if is_http_url(&url) {
        body["url"] = json!(url);
    }
    Some(body)
}

fn sphfeed_display_text(body: &Value) -> String {
    finder_display_text(
        body["sph_name"].as_str().unwrap_or(""),
        body["feed_desc"].as_str().unwrap_or(""),
    )
}

fn finder_display_text(nickname: &str, desc: &str) -> String {
    if !nickname.is_empty() && !desc.is_empty() {
        format!("[视频号] {nickname}: {desc}")
    } else if !desc.is_empty() {
        format!("[视频号] {desc}")
    } else if !nickname.is_empty() {
        format!("[视频号] {nickname}")
    } else {
        "[视频号]".to_string()
    }
}

fn xml_clean_text(xml: &str, tag: &str) -> String {
    clean_xml_text(extract_xml_text(xml, tag).unwrap_or_default())
}

fn xml_i64(xml: &str, tag: &str) -> Option<i64> {
    xml_clean_text(xml, tag).parse::<i64>().ok()
}

fn clean_xml_text(s: String) -> String {
    unescape_html(strip_xml_cdata(s.trim())).trim().to_string()
}

fn is_text_message(t: i64) -> bool {
    base_type(t) == 1
}

fn appmsg_url_for_content(content: &str) -> Option<String> {
    let url = extract_xml_text(content, "url")
        .or_else(|| extract_xml_text(content, "url1"))
        .map(|s| unescape_html(strip_xml_cdata(&s)))?;
    if url.starts_with("http://") || url.starts_with("https://") {
        Some(url)
    } else {
        None
    }
}

fn extract_xml_text(xml: &str, tag: &str) -> Option<String> {
    let open = format!("<{tag}>");
    let close = format!("</{tag}>");
    let start = xml.find(&open)?;
    let content_start = start + open.len();
    let end = xml[content_start..].find(&close)?;
    Some(xml[content_start..content_start + end].trim().to_string())
}

fn strip_xml_cdata(s: &str) -> &str {
    s.strip_prefix("<![CDATA[")
        .and_then(|inner| inner.strip_suffix("]]>"))
        .unwrap_or(s)
}

fn unescape_html(s: &str) -> String {
    s.replace("&amp;", "&")
        .replace("&lt;", "<")
        .replace("&gt;", ">")
        .replace("&quot;", "\"")
        .replace("&apos;", "'")
}

fn is_message_shard_key(rel_key: &str) -> bool {
    rel_key
        .rsplit('/')
        .next()
        .map(is_message_shard)
        .unwrap_or(false)
}

fn is_message_shard(file_name: &str) -> bool {
    if !file_name.starts_with("message_") || !file_name.ends_with(".db") {
        return false;
    }
    if file_name.contains("_fts") || file_name.contains("_resource") {
        return false;
    }
    let stem = &file_name["message_".len()..file_name.len() - ".db".len()];
    !stem.is_empty() && stem.chars().all(|c| c.is_ascii_digit())
}

fn full_decrypt(db_path: &Path, out_path: &Path, enc_key: &[u8; 32]) -> Result<()> {
    if let Some(parent) = out_path.parent() {
        fs::create_dir_all(parent)?;
    }
    let mut input = fs::File::open(db_path)?;
    let file_size = input.metadata()?.len() as usize;
    if file_size == 0 {
        bail!("数据库文件为空: {}", db_path.display());
    }
    let mut output = fs::File::create(out_path)?;
    let total_pages = file_size.div_ceil(PAGE_SZ);
    let mut page_buf = vec![0u8; PAGE_SZ];
    for pgno in 1..=total_pages {
        let page_start = (pgno - 1) * PAGE_SZ;
        let bytes_remaining = file_size.saturating_sub(page_start);
        let expected = bytes_remaining.min(PAGE_SZ);
        input.read_exact(&mut page_buf[..expected])?;
        if expected < PAGE_SZ {
            page_buf[expected..].fill(0);
        }
        let dec = decrypt_page(enc_key, &page_buf, pgno as u32)?;
        output.write_all(&dec)?;
    }
    Ok(())
}

fn apply_wal(wal_path: &Path, out_path: &Path, enc_key: &[u8; 32]) -> Result<()> {
    if !wal_path.exists() {
        return Ok(());
    }
    let wal_data = fs::read(wal_path)?;
    if wal_data.len() <= WAL_HDR_SZ {
        return Ok(());
    }
    let s1 = u32::from_be_bytes(wal_data[16..20].try_into().unwrap());
    let s2 = u32::from_be_bytes(wal_data[20..24].try_into().unwrap());
    let frame_size = WAL_FRAME_HDR + PAGE_SZ;
    let frame_area = &wal_data[WAL_HDR_SZ..];
    let mut db_file = fs::OpenOptions::new()
        .read(true)
        .write(true)
        .open(out_path)?;
    let mut pos = 0usize;
    while pos + frame_size <= frame_area.len() {
        let fh = &frame_area[pos..pos + WAL_FRAME_HDR];
        let page_data = &frame_area[pos + WAL_FRAME_HDR..pos + frame_size];
        let pgno = u32::from_be_bytes(fh[0..4].try_into().unwrap());
        let fs1 = u32::from_be_bytes(fh[8..12].try_into().unwrap());
        let fs2 = u32::from_be_bytes(fh[12..16].try_into().unwrap());
        pos += frame_size;
        if pgno == 0 || pgno > 1_000_000 || fs1 != s1 || fs2 != s2 {
            continue;
        }
        let mut page_buf = page_data.to_vec();
        if page_buf.len() < PAGE_SZ {
            page_buf.resize(PAGE_SZ, 0);
        }
        let dec = decrypt_page(enc_key, &page_buf, if pgno == 1 { 2 } else { pgno })?;
        let file_offset = (pgno as u64 - 1) * PAGE_SZ as u64;
        db_file.seek(SeekFrom::Start(file_offset))?;
        db_file.write_all(&dec)?;
    }
    Ok(())
}

fn decrypt_page(enc_key: &[u8; 32], page_data: &[u8], pgno: u32) -> Result<Vec<u8>> {
    if page_data.len() < PAGE_SZ {
        bail!("页面数据不足 {} 字节", PAGE_SZ);
    }
    let iv_offset = PAGE_SZ - RESERVE_SZ;
    let iv: &[u8; 16] = page_data[iv_offset..iv_offset + 16].try_into().unwrap();
    let mut result = vec![0u8; PAGE_SZ];
    if pgno == 1 {
        let enc = &page_data[SALT_SZ..PAGE_SZ - RESERVE_SZ];
        let dec = aes_cbc_decrypt(enc_key, iv, enc)?;
        result[..16].copy_from_slice(SQLITE_HDR);
        result[16..PAGE_SZ - RESERVE_SZ].copy_from_slice(&dec);
    } else {
        let enc = &page_data[..PAGE_SZ - RESERVE_SZ];
        let dec = aes_cbc_decrypt(enc_key, iv, enc)?;
        result[..PAGE_SZ - RESERVE_SZ].copy_from_slice(&dec);
    }
    Ok(result)
}

fn aes_cbc_decrypt(key: &[u8; 32], iv: &[u8; 16], data: &[u8]) -> Result<Vec<u8>> {
    if data.is_empty() || !data.len().is_multiple_of(16) {
        bail!("密文长度不是 AES 块大小的倍数: {}", data.len());
    }
    let mut blocks: Vec<Block> = data.chunks_exact(16).map(Block::clone_from_slice).collect();
    Aes256CbcDec::new(key.into(), iv.into()).decrypt_blocks_mut(&mut blocks);
    Ok(blocks.iter().flat_map(|b| b.iter().copied()).collect())
}

fn wal_path_for(db_path: &Path) -> PathBuf {
    let mut name = db_path.file_name().unwrap_or_default().to_os_string();
    name.push("-wal");
    db_path.with_file_name(name)
}

fn mtime_nanos(path: &Path) -> u64 {
    fs::metadata(path)
        .and_then(|m| m.modified())
        .map(|t| t.duration_since(UNIX_EPOCH).unwrap_or_default().as_nanos() as u64)
        .unwrap_or(0)
}

fn hex_to_32bytes(s: &str) -> Result<[u8; 32]> {
    if s.len() != 64 {
        bail!("密钥 hex 长度应为 64，实际为 {}", s.len());
    }
    let mut out = [0u8; 32];
    for i in 0..32 {
        out[i] = u8::from_str_radix(&s[i * 2..i * 2 + 2], 16)
            .with_context(|| format!("非法 hex 字符 at {}", i * 2))?;
    }
    Ok(out)
}

fn hex_encode(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn message_query_advances_outgoing_rows_but_only_emits_incoming_rows() {
        let path = std::env::temp_dir().join(format!("webox-message-{}.db", uuid::Uuid::new_v4()));
        let conn = Connection::open(&path).unwrap();
        conn.execute_batch(
            "CREATE TABLE [Msg_test] (
                local_id INTEGER PRIMARY KEY,
                server_id INTEGER,
                local_type INTEGER,
                create_time INTEGER,
                real_sender_id INTEGER,
                message_content TEXT,
                WCDB_CT_message_content INTEGER,
                status INTEGER,
                origin_source INTEGER
            );
            INSERT INTO [Msg_test] VALUES (1, 101, 1, 1000, 0, 'outgoing', 0, 2, 1);
            INSERT INTO [Msg_test] VALUES (2, 102, 1, 1000, 0, 'incoming', 0, 3, 2);",
        )
        .unwrap();
        drop(conn);

        let events = query_new_table(
            &path,
            "Msg_test",
            "message/msg_0.db",
            "alice",
            false,
            MessagePosition {
                create_time: 999,
                local_id: 0,
            },
            100,
        )
        .unwrap();

        assert_eq!(events.len(), 2);
        assert!(events[0].message.is_none());
        assert_eq!(events[0].position.local_id, 1);
        assert_eq!(
            events[1].message.as_ref().unwrap()["text"]["content"],
            "incoming"
        );
        assert_eq!(events[1].position.local_id, 2);
        fs::remove_file(path).ok();
    }

    #[test]
    fn message_query_uses_local_id_to_resume_within_same_second() {
        let path = std::env::temp_dir().join(format!("webox-message-{}.db", uuid::Uuid::new_v4()));
        let conn = Connection::open(&path).unwrap();
        conn.execute_batch(
            "CREATE TABLE [Msg_test] (
                local_id INTEGER PRIMARY KEY, server_id INTEGER, local_type INTEGER,
                create_time INTEGER, real_sender_id INTEGER, message_content TEXT,
                WCDB_CT_message_content INTEGER, status INTEGER, origin_source INTEGER
            );
            INSERT INTO [Msg_test] VALUES (1, 101, 1, 1000, 0, 'first', 0, 3, 2);
            INSERT INTO [Msg_test] VALUES (2, 102, 1, 1000, 0, 'second', 0, 3, 2);",
        )
        .unwrap();
        drop(conn);

        let events = query_new_table(
            &path,
            "Msg_test",
            "message/msg_0.db",
            "alice",
            false,
            MessagePosition {
                create_time: 1000,
                local_id: 1,
            },
            100,
        )
        .unwrap();

        assert_eq!(events.len(), 1);
        assert_eq!(events[0].position.local_id, 2);
        fs::remove_file(path).ok();
    }

    #[test]
    fn recipient_requires_a_remark_and_keeps_internal_username() {
        let recipient =
            recipient_from_remark("24933085811@chatroom".to_string(), "唯一备注".to_string())
                .unwrap();
        assert_eq!(recipient.username, "24933085811@chatroom");
        assert_eq!(recipient.search_term, "唯一备注");

        assert!(recipient_from_remark("wxid_test".to_string(), String::new()).is_none());
    }

    #[test]
    fn recipient_display_prefers_remark_then_nick_then_alias() {
        assert_eq!(
            recipient_display_name("wxid_1", "昵称", "备注", "alias"),
            "备注"
        );
        assert_eq!(
            recipient_display_name("wxid_1", "昵称", "", "alias"),
            "昵称"
        );
        assert_eq!(recipient_display_name("wxid_1", "", "", "alias"), "alias");
        assert_eq!(recipient_display_name("wxid_1", "", "", ""), "wxid_1");
    }

    #[test]
    fn extracts_resource_md5_from_packed_info() {
        let mut blob = vec![0xAA, 0xBB];
        blob.extend_from_slice(&[0x12, 0x22, 0x0A, 0x20]);
        blob.extend_from_slice(b"DEADBEEFCAFEBABE1234567890ABCDEF");

        assert_eq!(
            extract_resource_md5_from_packed_info(&blob),
            Some("deadbeefcafebabe1234567890abcdef".to_string())
        );
    }

    #[test]
    fn detects_extended_image_content_types() {
        assert_eq!(
            detect_media_content_type(&[0xFF, 0xD8, 0xFF, 0xE0]),
            "image/jpeg"
        );
        assert_eq!(detect_media_content_type(b"GIF89a"), "image/gif");
        assert_eq!(detect_media_content_type(b"wxgfxxxx"), "image/heic");
    }

    #[test]
    fn detects_mp4_video_content_type() {
        assert_eq!(
            detect_media_content_type(b"\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00"),
            "video/mp4"
        );
        assert_eq!(extension_for_content_type("video/mp4"), "mp4");
    }

    #[test]
    fn finds_local_video_by_xml_md5() {
        let root = std::env::temp_dir().join(format!("webox-video-test-{}", uuid::Uuid::new_v4()));
        let account_dir = root.join("account");
        let video_dir = account_dir.join("msg").join("video").join("2026-06");
        fs::create_dir_all(&video_dir).unwrap();
        let md5 = "0123456789abcdef0123456789abcdef";
        let data = b"\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00video";
        fs::write(video_dir.join(format!("{md5}.mp4")), data).unwrap();

        let media = find_video_file(
            &account_dir,
            7,
            1781703356,
            &format!(r#"<videomsg md5="{md5}" />"#),
        )
        .unwrap()
        .unwrap();

        assert_eq!(media.content_type, "video/mp4");
        assert_eq!(media.filename, format!("{md5}.mp4"));
        assert_eq!(media.data, data);
        let _ = fs::remove_dir_all(root);
    }

    #[test]
    fn extracts_wxgf_hevc_payload() {
        let data = b"wxgf private header\x00\x00\x00\x01\x40\x01payload";
        assert_eq!(
            wxgf_hevc_payload(data).unwrap(),
            b"\x00\x00\x00\x01\x40\x01payload"
        );
        assert!(wxgf_hevc_payload(b"\x00\x00\x00\x01\x40\x01payload").is_none());
    }

    #[test]
    fn extracts_xml_attrs_with_optional_spaces() {
        let xml = r#"<emoji androidmd5="bad" md5 = "c178cf89290291bb49023c0096ba57d6" cdnurl = "http://x.test/a?m=1&amp;n=2" aeskey= "0f0ef2e6b33a4713a560c1b0b1f6546b" />"#;
        assert_eq!(
            xml_attr(xml, "md5").as_deref(),
            Some("c178cf89290291bb49023c0096ba57d6")
        );
        assert_eq!(
            xml_attr(xml, "cdnurl").as_deref(),
            Some("http://x.test/a?m=1&n=2")
        );
        assert_eq!(
            xml_attr(xml, "aeskey").as_deref(),
            Some("0f0ef2e6b33a4713a560c1b0b1f6546b")
        );
    }

    #[test]
    fn decodes_v2_image_segments() {
        use aes::cipher::BlockEncrypt;

        let key: [u8; 16] = *b"0123456789abcdef";
        let cipher = Aes128::new((&key).into());
        let mut plain = b"\xFF\xD8\xFF\xE0".to_vec();
        plain.extend_from_slice(&[12u8; 12]);
        let mut block = GenericArray::clone_from_slice(&plain);
        cipher.encrypt_block(&mut block);

        let xor_key = 0xEF;
        let mut dat = V2_IMAGE_MAGIC.to_vec();
        dat.extend_from_slice(&4u32.to_le_bytes());
        dat.extend_from_slice(&3u32.to_le_bytes());
        dat.push(0);
        dat.extend_from_slice(&block);
        dat.extend_from_slice(b"raw");
        dat.extend([b'a' ^ xor_key, b'b' ^ xor_key, b'c' ^ xor_key]);

        assert_eq!(
            decode_v2_image(&dat, &key, xor_key).unwrap(),
            b"\xFF\xD8\xFF\xE0rawabc"
        );
    }

    #[test]
    fn normalizes_wxid_with_suffix() {
        assert_eq!(normalize_wxid("wxid_abc_def0"), "wxid_abc");
        assert_eq!(normalize_wxid("plain_abcd"), "plain");
    }

    #[test]
    fn appmsg_quote_keeps_referenced_message() {
        let xml = r#"
<?xml version="1.0"?>
<msg>
  <appmsg appid="" sdkver="0">
    <title>@私云虾虾</title>
    <type>57</type>
    <refermsg>
      <chatusr>che006</chatusr>
      <type>1</type>
      <createtime>1781610131</createtime>
      <displayname>青椒Der(大学青年教师，不是大师)</displayname>
      <svrid>7318462845630259071</svrid>
      <fromusr>24933085811@chatroom</fromusr>
      <content>虾虾，这个视频的标题是什么：https://www.youtube.com/watch?v=P3W5L3HGBgg</content>
    </refermsg>
  </appmsg>
</msg>
"#;

        let quote = parse_refermsg(xml).expect("quote should parse");

        assert_eq!(quote.chatusr, "che006");
        assert_eq!(quote.displayname, "青椒Der(大学青年教师，不是大师)");
        assert_eq!(quote.msgid, "7318462845630259071");
        assert_eq!(
            quote.content,
            "虾虾，这个视频的标题是什么：https://www.youtube.com/watch?v=P3W5L3HGBgg"
        );
        assert_eq!(quote.msgtype, "text");
        assert_eq!(
            quote_to_json(&quote),
            json!({
                "msgtype": "text",
                "msgid": "7318462845630259071",
                "from": "青椒Der(大学青年教师，不是大师)",
                "text": {
                    "content": "虾虾，这个视频的标题是什么：https://www.youtube.com/watch?v=P3W5L3HGBgg"
                }
            })
        );
        assert_eq!(parse_appmsg(xml), Some("@私云虾虾".to_string()));
    }

    #[test]
    fn appmsg_quote_formats_referenced_image_without_raw_xml() {
        let xml = r#"
<msg>
  <appmsg>
    <title>虾虾，这个现在就是你的头像，喜欢吗？</title>
    <type>57</type>
    <refermsg>
      <displayname>小金鱼</displayname>
      <content>&lt;?xml version=&quot;1.0&quot;?&gt;&lt;msg&gt;&lt;img md5=&quot;e7339e018cd24be9d4631b24d05359b2&quot;/&gt;&lt;/msg&gt;</content>
    </refermsg>
  </appmsg>
</msg>
"#;

        let quote = parse_refermsg(xml).expect("quote should parse");

        assert_eq!(
            parse_appmsg(xml),
            Some("虾虾，这个现在就是你的头像，喜欢吗？".to_string())
        );
        assert_eq!(
            quote_to_json(&quote),
            json!({
                "msgtype": "image",
                "from": "小金鱼",
                "image": {
                    "content": "[图片]"
                }
            })
        );
    }

    #[test]
    fn appmsg_quote_formats_group_prefixed_referenced_image_without_raw_xml() {
        let xml = r#"
<msg>
  <appmsg>
    <title>虾虾，这个现在就是你的头像，喜欢吗？</title>
    <type>57</type>
    <refermsg>
      <displayname>小金鱼</displayname>
      <content>wxid_74wgcpw5yswo12:
&lt;?xml version=&quot;1.0&quot;?&gt;&lt;msg&gt;&lt;img md5=&quot;e7339e018cd24be9d4631b24d05359b2&quot;/&gt;&lt;/msg&gt;</content>
    </refermsg>
  </appmsg>
</msg>
"#;

        let quote = parse_refermsg(xml).expect("quote should parse");

        assert_eq!(
            quote_to_json(&quote),
            json!({
                "msgtype": "image",
                "from": "小金鱼",
                "image": {
                    "content": "[图片]"
                }
            })
        );
    }

    #[test]
    fn appmsg_quote_formats_nested_reply_content_without_raw_xml() {
        let xml = r#"
<msg>
  <appmsg>
    <title>我的菜基本上都是凌晨三四点熟的</title>
    <type>57</type>
    <refermsg>
      <displayname>肖淑洁的千瑜</displayname>
      <content>&lt;?xml version=&quot;1.0&quot;?&gt;&lt;msg&gt;&lt;appmsg&gt;&lt;title&gt;你等她下播，再去催熟&lt;/title&gt;&lt;type&gt;57&lt;/type&gt;&lt;refermsg&gt;&lt;/refermsg&gt;&lt;/appmsg&gt;&lt;/msg&gt;</content>
    </refermsg>
  </appmsg>
</msg>
"#;

        let quote = parse_refermsg(xml).expect("quote should parse");

        assert_eq!(
            parse_appmsg(xml),
            Some("我的菜基本上都是凌晨三四点熟的".to_string())
        );
        assert_eq!(
            quote_to_json(&quote),
            json!({
                "msgtype": "text",
                "from": "肖淑洁的千瑜",
                "text": {
                    "content": "你等她下播，再去催熟"
                }
            })
        );
    }

    #[test]
    fn appmsg_finder_feed_formats_as_sphfeed_body() {
        let xml = r#"
<msg>
  <appmsg appid="" sdkver="0">
    <title>当前微信版本不支持展示该内容，请升级至最新版本。</title>
    <type>51</type>
    <url>https://support.weixin.qq.com/update/</url>
    <finderFeed>
      <objectId><![CDATA[14943728025064905116]]></objectId>
      <feedType><![CDATA[4]]></feedType>
      <nickname><![CDATA[么会良-刑事律师]]></nickname>
      <desc><![CDATA[同样搭梯子，为什么有人判刑有人没事？#搭梯子#VPN法律问题]]></desc>
      <mediaCount><![CDATA[1]]></mediaCount>
      <objectNonceId><![CDATA[15933239367236431539_0]]></objectNonceId>
      <username><![CDATA[v2_abc@finder]]></username>
      <mediaList>
        <media>
          <thumbUrl><![CDATA[https://wxapp.tc.qq.com/thumb.jpg]]></thumbUrl>
          <videoPlayDuration><![CDATA[303]]></videoPlayDuration>
          <url><![CDATA[http://wxapp.tc.qq.com/video.mp4]]></url>
          <coverUrl><![CDATA[https://wxapp.tc.qq.com/cover.jpg]]></coverUrl>
          <height><![CDATA[1920]]></height>
          <mediaType><![CDATA[4]]></mediaType>
          <width><![CDATA[1080]]></width>
        </media>
      </mediaList>
    </finderFeed>
  </appmsg>
</msg>
"#;

        let body = appmsg_sphfeed_body(xml).expect("finder feed should parse");

        assert_eq!(body["feed_type"], 4);
        assert_eq!(body["sph_name"], "么会良-刑事律师");
        assert_eq!(
            body["feed_desc"],
            "同样搭梯子，为什么有人判刑有人没事？#搭梯子#VPN法律问题"
        );
        assert_eq!(body["url"], "http://wxapp.tc.qq.com/video.mp4");
        assert_eq!(
            sphfeed_display_text(&body),
            "[视频号] 么会良-刑事律师: 同样搭梯子，为什么有人判刑有人没事？#搭梯子#VPN法律问题"
        );
    }

    #[test]
    fn appmsg_file_is_classified_as_file() {
        let xml = r#"<msg><appmsg><title>report.pdf</title><type>6</type></appmsg></msg>"#;

        assert_eq!(appmsg_type(xml), Some(6));
        assert_eq!(msgtype_for_message(49, xml, false, false), "file");
    }

    #[test]
    fn filehelper_uses_the_stable_builtin_search_name() {
        let recipient = resolve_recipient_by_username(
            PathBuf::new(),
            HashMap::new(),
            PathBuf::new(),
            "filehelper",
            "wxid_self",
        )
        .unwrap()
        .unwrap();

        assert_eq!(recipient.username, "filehelper");
        assert_eq!(recipient.search_term, "文件传输助手");
    }

    #[test]
    #[ignore = "requires a live WeChat database"]
    fn live_current_user_is_present_in_contacts() {
        let db_dir = PathBuf::from(std::env::var("WEBOX_LIVE_DB_DIR").unwrap());
        let key_file = PathBuf::from(std::env::var("WEBOX_LIVE_KEY_FILE").unwrap());
        let user_id = std::env::var("WEBOX_LIVE_USER_ID").unwrap();
        let key_doc: Value = serde_json::from_slice(&fs::read(key_file).unwrap()).unwrap();
        let keys: HashMap<String, String> = key_doc["keys"]
            .as_object()
            .unwrap()
            .iter()
            .map(|(name, value)| (name.clone(), value.as_str().unwrap().to_string()))
            .collect();
        let cache_dir =
            std::env::temp_dir().join(format!("webox-self-contact-live-{}", uuid::Uuid::new_v4()));

        let identity =
            contact_identity(db_dir.clone(), keys.clone(), cache_dir.clone(), &user_id).unwrap();
        let recipient =
            resolve_recipient_by_username(db_dir, keys, cache_dir.clone(), &user_id, &user_id)
                .unwrap();

        fs::remove_dir_all(cache_dir).ok();
        assert!(
            identity.is_some(),
            "current user is missing from contact.db"
        );
        assert!(recipient.is_some(), "current user has no UI search term");
    }

    #[test]
    #[ignore = "requires a live WeChat database"]
    fn live_outgoing_history_contains_probes() {
        let db_dir = PathBuf::from(std::env::var("WEBOX_LIVE_DB_DIR").unwrap());
        let key_file = PathBuf::from(std::env::var("WEBOX_LIVE_KEY_FILE").unwrap());
        let room_id =
            std::env::var("WEBOX_LIVE_ROOM_ID").unwrap_or_else(|_| "filehelper".to_string());
        let key_doc: Value = serde_json::from_slice(&fs::read(key_file).unwrap()).unwrap();
        let keys: HashMap<String, String> = key_doc["keys"]
            .as_object()
            .unwrap()
            .iter()
            .map(|(name, value)| (name.clone(), value.as_str().unwrap().to_string()))
            .collect();
        let cache_dir =
            std::env::temp_dir().join(format!("webox-filehelper-live-{}", uuid::Uuid::new_v4()));
        for probe in std::env::var("WEBOX_LIVE_EXPECTED_TEXTS")
            .unwrap()
            .split('|')
        {
            assert!(
                outgoing_text_contains(
                    db_dir.clone(),
                    keys.clone(),
                    cache_dir.clone(),
                    &room_id,
                    probe,
                )
                .unwrap(),
                "{room_id} history is missing probe {probe}"
            );
        }
        fs::remove_dir_all(cache_dir).ok();
    }

    #[test]
    fn appmsg_quote_formats_finder_feed_as_sphfeed_without_raw_xml() {
        let xml = r#"
<msg>
  <appmsg>
    <title>虾虾，这个视频讲了啥？</title>
    <type>57</type>
    <refermsg>
      <displayname>小金鱼</displayname>
      <svrid>1885904539416357140</svrid>
      <content>&lt;msg&gt;&lt;appmsg&gt;&lt;title&gt;当前微信版本不支持展示该内容，请升级至最新版本。&lt;/title&gt;&lt;type&gt;51&lt;/type&gt;&lt;url&gt;https://support.weixin.qq.com/update/&lt;/url&gt;&lt;finderFeed&gt;&lt;nickname&gt;&lt;![CDATA[么会良-刑事律师]]&gt;&lt;/nickname&gt;&lt;desc&gt;&lt;![CDATA[同样搭梯子，为什么有人判刑有人没事？]]&gt;&lt;/desc&gt;&lt;mediaList&gt;&lt;media&gt;&lt;url&gt;&lt;![CDATA[http://wxapp.tc.qq.com/video.mp4]]&gt;&lt;/url&gt;&lt;coverUrl&gt;&lt;![CDATA[https://wxapp.tc.qq.com/cover.jpg]]&gt;&lt;/coverUrl&gt;&lt;mediaType&gt;4&lt;/mediaType&gt;&lt;/media&gt;&lt;/mediaList&gt;&lt;/finderFeed&gt;&lt;/appmsg&gt;&lt;/msg&gt;</content>
    </refermsg>
  </appmsg>
</msg>
"#;

        let quote = parse_refermsg(xml).expect("quote should parse");
        let quoted = quote_to_json(&quote);

        assert_eq!(quote.msgtype, "sphfeed");
        assert_eq!(quoted["msgid"], "1885904539416357140");
        assert_eq!(quoted["from"], "小金鱼");
        assert_eq!(quoted["sphfeed"]["feed_type"], 4);
        assert_eq!(quoted["sphfeed"]["sph_name"], "么会良-刑事律师");
        assert_eq!(
            quoted["sphfeed"]["feed_desc"],
            "同样搭梯子，为什么有人判刑有人没事？"
        );
        assert_eq!(quoted["sphfeed"]["url"], "http://wxapp.tc.qq.com/video.mp4");
    }
}
