use anyhow::{anyhow, Result};
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use hmac::{Hmac, Mac};
use serde::de::DeserializeOwned;
use serde::Serialize;
use sha2::Sha256;

type HmacSha256 = Hmac<Sha256>;

pub fn encode<T: Serialize>(key: &str, value: &T) -> Result<String> {
    let payload = URL_SAFE_NO_PAD.encode(serde_json::to_vec(value)?);
    let mut mac = HmacSha256::new_from_slice(key.as_bytes())?;
    mac.update(payload.as_bytes());
    let signature = URL_SAFE_NO_PAD.encode(mac.finalize().into_bytes());
    Ok(format!("{payload}.{signature}"))
}

pub fn decode<T: DeserializeOwned>(key: &str, token: &str) -> Result<T> {
    let (payload, signature) = token
        .trim()
        .split_once('.')
        .ok_or_else(|| anyhow!("missing signature"))?;
    let signature = URL_SAFE_NO_PAD.decode(signature)?;
    let mut mac = HmacSha256::new_from_slice(key.as_bytes())?;
    mac.update(payload.as_bytes());
    mac.verify_slice(&signature)
        .map_err(|_| anyhow!("signature mismatch"))?;
    let bytes = URL_SAFE_NO_PAD.decode(payload)?;
    Ok(serde_json::from_slice(&bytes)?)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde::{Deserialize, Serialize};

    #[derive(Debug, Deserialize, PartialEq, Serialize)]
    struct Payload {
        value: String,
    }

    #[test]
    fn signed_payload_round_trips_and_rejects_tampering() {
        let value = Payload {
            value: "hello".to_string(),
        };
        let token = encode("secret", &value).unwrap();

        assert_eq!(decode::<Payload>("secret", &token).unwrap(), value);

        let mut tampered = token.into_bytes();
        tampered[0] = if tampered[0] == b'a' { b'b' } else { b'a' };
        assert!(decode::<Payload>("secret", std::str::from_utf8(&tampered).unwrap()).is_err());
    }
}
