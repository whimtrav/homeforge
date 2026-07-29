#!/usr/bin/env python3
"""Kidde HomeSafe -> HomeForge MQTT bridge.

Polls the Kidde HomeSafe cloud (via the kidde-homesafe lib) and republishes each
detector's alarm/sensor state to kidde/<device>/<key> (retained). HomeForge's
handleKidde (kidde/+/+) turns those into binary_sensor.kidde_* / sensor.kidde_*.

Auth: prefers email+password (self-refreshing) and falls back to the stored
session cookie. Read-only — test/hush stay in the Kidde app.
"""
import asyncio
import json
import logging
import os
import sys

import paho.mqtt.client as mqtt
from kidde_homesafe import KiddeClient, KiddeClientAuthError

logging.basicConfig(level=logging.INFO, format="%(asctime)s kidde: %(message)s")
log = logging.getLogger("kidde")

MQTT_HOST = os.environ.get("MQTT_HOST", "127.0.0.1")
MQTT_PORT = int(os.environ.get("MQTT_PORT", "1885"))
MQTT_USER = os.environ.get("MQTT_USER", "homeforge")
MQTT_PASS = os.environ.get("MQTT_PASS", "homeforge")
POLL = int(os.environ.get("POLL_SECONDS", "30"))

KIDDE_EMAIL = os.environ.get("KIDDE_EMAIL", "").strip()
KIDDE_PASSWORD = os.environ.get("KIDDE_PASSWORD", "").strip()
KIDDE_COOKIE = os.environ.get("KIDDE_COOKIE", "").strip()

# Keys to publish. Binary keys (bool) become on/off; others are published raw
# (measurement dicts {value,status,Unit} are unwrapped to .value).
BINARY_KEYS = [
    "smoke_alarm", "co_alarm", "hardwire_smoke", "too_much_smoke",
    "water_alarm", "low_temp_alarm", "low_battery_alarm", "smoke_hushed",
]
SENSOR_KEYS = [
    "temperature", "humidity", "iaq_temperature", "tvoc", "co2", "iaq", "hpa",
    "smoke_level", "co_level", "batt_volt", "battery_voltage", "battery_level",
    "life", "ap_rssi", "overall_iaq_status", "ssid", "model", "last_seen",
]


def slug(s: str) -> str:
    out, prev = [], False
    for ch in str(s).lower():
        if ch.isalnum():
            out.append(ch)
            prev = False
        elif not prev:
            out.append("_")
            prev = True
    return "".join(out).strip("_") or "device"


def resolve(v):
    """Turn a device value into an MQTT string payload, or None to skip."""
    if v is None:
        return None
    if isinstance(v, bool):
        return "on" if v else "off"
    if isinstance(v, dict):
        inner = v.get("value")
        return None if inner is None else str(inner)
    return str(v)


def mqtt_connect() -> mqtt.Client:
    c = mqtt.Client()
    c.username_pw_set(MQTT_USER, MQTT_PASS)
    c.connect(MQTT_HOST, MQTT_PORT, 60)
    c.loop_start()
    log.info("MQTT -> %s:%s", MQTT_HOST, MQTT_PORT)
    return c


async def make_client() -> KiddeClient:
    if KIDDE_EMAIL and KIDDE_PASSWORD:
        try:
            client = await KiddeClient.from_login(KIDDE_EMAIL, KIDDE_PASSWORD)
            log.info("logged in as %s (session self-refreshing)", KIDDE_EMAIL)
            return client
        except KiddeClientAuthError:
            log.warning("email/password rejected — falling back to session cookie")
        except Exception as e:  # noqa: BLE001
            log.warning("login error (%s) — falling back to session cookie", type(e).__name__)
    if KIDDE_COOKIE:
        log.info("using stored session cookie")
        return KiddeClient({"session": KIDDE_COOKIE})
    log.error("no usable credentials (need KIDDE_EMAIL+KIDDE_PASSWORD or KIDDE_COOKIE)")
    sys.exit(1)


def publish_devices(m: mqtt.Client, devices: dict) -> int:
    n = 0
    for dev_id, dev in devices.items():
        name = slug(dev.get("label") or dev.get("id") or dev_id)
        # connectivity (invert 'offline')
        if "offline" in dev:
            m.publish(f"kidde/{name}/online", "off" if dev.get("offline") else "on", retain=True)
            n += 1
        for key in BINARY_KEYS + SENSOR_KEYS:
            if key not in dev:
                continue
            val = resolve(dev.get(key))
            if val is None:
                continue
            m.publish(f"kidde/{name}/{key}", val, retain=True)
            n += 1
    return n


async def main():
    m = mqtt_connect()
    client = await make_client()
    while True:
        try:
            data = await client.get_data(get_events=False)
            count = publish_devices(m, data.devices)
            log.info("published %d values across %d devices", count, len(data.devices))
        except KiddeClientAuthError:
            log.warning("auth expired — re-authenticating")
            client = await make_client()
            continue
        except Exception as e:  # noqa: BLE001
            log.warning("poll error: %s: %s", type(e).__name__, e)
        await asyncio.sleep(POLL)


if __name__ == "__main__":
    asyncio.run(main())
