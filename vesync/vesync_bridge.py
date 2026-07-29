#!/usr/bin/env python3
"""VeSync <-> HomeForge bridge (pyvesync 2.x, async).

STATE OUT: polls the VeSync cloud every POLL sec, publishes retained MQTT
  vesync/<room>/<metric>  ->  HomeForge handleVeSync -> sensor/switch/number entities.
  Control aliases published so HF can build controls:
    power           -> switch  (on/off)
    fan             -> number  (0=auto, 1-4 manual)   [air purifier]
    humidity_target -> number  (40-80% target)         [humidifiers]
COMMANDS IN: subscribes vesync/<room>/set  {"power":"on"|"off"} / {"fan":N} / {"humidity":N}
  -> calls the pyvesync method, then republishes that device's state.
SCALE: Etekcity ESF24 (Bluetooth) last weigh-in lives in the account profile
  (getUserInfo -> weightG); we compute weight/BMI/age and POST to /api/health too.
"""
import asyncio
import inspect
import json
import time
from datetime import date, datetime
import os

import aiohttp
import paho.mqtt.client as mqtt
from pyvesync import VeSync

USER = os.environ["VESYNC_USER"]
PASS = os.environ["VESYNC_PASS"]
MQTT_HOST = os.environ.get("MQTT_HOST", "127.0.0.1")
MQTT_PORT = int(os.environ.get("MQTT_PORT", "1885"))
MQTT_USER = os.environ.get("MQTT_USER", "homeforge")
MQTT_PASS = os.environ.get("MQTT_PASS", "homeforge")
HF_API = os.environ.get("HF_API", "http://127.0.0.1:8093")
POLL = int(os.environ.get("POLL_SEC", "120"))

# Room-based rename map (VeSync cloud names are wrong after devices were physically moved).
# Keyed on the stripped, lowercased VeSync device_name. Edit + redeploy when devices move.
NAME_MAP = {
    "house humidifier": "familyroom-humidifier",        # big/evaporative, upstairs family room
    "dining room humidifier": "livingroom-humidifier",  # small/ultrasonic, downstairs living room
    "living room air purifier": "diningroom-purifier",  # the smart LAP-V102S, in the dining room
}

# Raw state fields NOT re-published as sensors (they're represented by control entities instead).
RAW_SKIP = {"device_status", "fan_level", "fan_set_level",
            "auto_target_humidity", "target_humidity", "last_update_ts"}

loop = None          # asyncio loop (set in run())
manager = None       # VeSync manager
device_map = {}      # sanitized-room-name -> pyvesync device (rebuilt each poll)


def sanitize(s):
    return "".join(c.lower() if c.isalnum() else "_" for c in str(s)).strip("_")


def room_name(raw):
    return NAME_MAP.get(str(raw).strip().lower(), str(raw).strip())


async def aw(x):
    return await x if inspect.isawaitable(x) else x


def age_from(birthday):
    try:
        b = datetime.strptime(birthday, "%Y/%m/%d").date()
        t = date.today()
        return t.year - b.year - ((t.month, t.day) < (b.month, b.day))
    except Exception:
        return None


# ---- MQTT ----
def on_connect(client, userdata, flags, rc):
    client.subscribe("vesync/+/set")
    print("vesync: subscribed vesync/+/set", flush=True)


def on_message(client, userdata, msg):
    parts = msg.topic.split("/")
    if len(parts) != 3 or parts[2] != "set":
        return
    dev = parts[1]
    try:
        payload = json.loads(msg.payload.decode() or "{}")
    except Exception:
        return
    if loop is not None and manager is not None:
        asyncio.run_coroutine_threadsafe(handle_cmd(dev, payload), loop)


cli = mqtt.Client(client_id="vesync-bridge")
cli.username_pw_set(MQTT_USER, MQTT_PASS)
cli.on_connect = on_connect
cli.on_message = on_message
cli.connect(MQTT_HOST, MQTT_PORT, 60)
cli.loop_start()
print(f"vesync: MQTT -> {MQTT_HOST}:{MQTT_PORT}", flush=True)


def pub(dev, metric, val):
    if val is None or isinstance(val, (dict, list)):
        return
    cli.publish(f"vesync/{sanitize(dev)}/{sanitize(metric)}", str(val), retain=True)


def state_fields(st):
    """Robustly extract scalar fields from a 2.x device state (slots/dataclass)."""
    fields = {}
    for getter in ("to_dict", "to_json_dict", "as_dict"):
        fn = getattr(st, getter, None)
        if callable(fn):
            try:
                fields = fn()
                break
            except Exception:
                fields = {}
    if not fields:
        for k in dir(st):
            if k.startswith("_"):
                continue
            try:
                v = getattr(st, k)
            except Exception:
                continue
            if not callable(v):
                fields[k] = v
    return fields


def publish_one(d):
    name = room_name(getattr(d, "device_name", "") or "")
    if not name:
        return
    st = d.state
    pub(name, "type", getattr(d, "device_type", ""))
    pub(name, "connection_status", getattr(d, "connection_status", ""))

    # --- control aliases ---
    ds = getattr(st, "device_status", None)
    if ds in ("on", "off"):
        pub(name, "power", ds)
    if hasattr(d, "set_fan_speed"):  # air purifier
        mode = getattr(st, "mode", None)
        fl = getattr(st, "fan_level", None)
        if mode == "auto":
            pub(name, "fan", 0)
        elif isinstance(fl, int):
            pub(name, "fan", fl)
    if hasattr(d, "set_humidity"):  # humidifier
        tgt = getattr(st, "auto_target_humidity", None) or getattr(st, "target_humidity", None)
        if isinstance(tgt, (int, float)) and tgt > 0:
            pub(name, "humidity_target", int(tgt))

    # --- remaining fields as sensors ---
    for k, v in state_fields(st).items():
        if k.startswith("_") or k in RAW_SKIP:
            continue
        if isinstance(v, (bool, int, float, str)):
            pub(name, k, v)


async def handle_cmd(dev_key, payload):
    d = device_map.get(dev_key)
    if d is None:
        print(f"vesync: cmd for unknown '{dev_key}' (known: {list(device_map)})", flush=True)
        return
    print(f"vesync: cmd {dev_key} {payload}", flush=True)
    try:
        if "power" in payload:
            on = str(payload["power"]).lower() in ("on", "true", "1")
            await aw(d.turn_on() if on else d.turn_off())
        if "fan" in payload:
            n = int(payload["fan"])
            if n <= 0:
                await aw(d.set_auto_mode())
            else:
                if hasattr(d, "set_manual_mode"):
                    await aw(d.set_manual_mode())
                await aw(d.set_fan_speed(n))
        if "humidity" in payload:
            n = int(payload["humidity"])
            await aw(d.set_humidity(n))
            if hasattr(d, "set_auto_mode"):
                await aw(d.set_auto_mode())
        await aw(manager.update())
        publish_one(d)
    except Exception as e:
        print("vesync: cmd error:", e, flush=True)


async def publish_devices(m):
    global device_map
    await aw(m.update())
    devs = list(getattr(m, "devices", []) or [])
    dm = {}
    for d in devs:
        raw = getattr(d, "device_name", "") or ""
        if not raw:
            continue
        dm[sanitize(room_name(raw))] = d
        publish_one(d)
    device_map = dm
    return len(devs)


async def publish_scale(m, session):
    base = m._api_base_url_for_current_region()
    headers = {"Content-Type": "application/json; charset=UTF-8", "User-Agent": "okhttp/3.12.1",
               "accountId": m.account_id, "tk": m.token, "tz": "America/Denver", "appVersion": "5.6.60"}
    body = {"timeZone": "America/Denver", "acceptLanguage": "en", "accountID": m.account_id,
            "token": m.token, "appVersion": "5.6.60", "phoneBrand": "pyvesync", "phoneOS": "Android",
            "traceId": str(int(time.time())), "userCountryCode": "US", "method": "getUserInfo"}
    try:
        async with session.post(base + "/cloud/v1/user/getUserInfo", json=body, headers=headers,
                                timeout=aiohttp.ClientTimeout(total=20)) as r:
            j = await r.json(content_type=None)
    except Exception as e:
        print("vesync: scale getUserInfo error:", e, flush=True)
        return
    res = (j or {}).get("result")
    if not res:
        return
    weight_g = res.get("weightG") or 0.0
    height_cm = res.get("heightCm") or 0.0
    health = {}
    if weight_g and weight_g > 0:
        kg = round(weight_g / 1000.0, 2)
        lb = round(kg * 2.2046226, 1)
        pub("scale", "weight_kg", kg)
        pub("scale", "weight_lb", lb)
        health["weight_lb"] = lb
        if height_cm and height_cm > 0:
            bmi = round(kg / ((height_cm / 100.0) ** 2), 1)
            pub("scale", "bmi", bmi)
            health["bmi"] = bmi
    if height_cm and height_cm > 0:
        pub("scale", "height_cm", round(height_cm, 1))
    tgt_lb = res.get("weightTargetLb") or 0.0
    if tgt_lb and tgt_lb > 0:
        pub("scale", "target_weight_lb", round(tgt_lb, 1))
    age = age_from(res.get("birthday", ""))
    if age:
        pub("scale", "age", age)
    pub("scale", "connection_status", "cloud")
    if health:
        try:
            async with session.post(f"{HF_API}/api/health", json=health,
                                    timeout=aiohttp.ClientTimeout(total=15)) as r:
                await r.read()
        except Exception as e:
            print("vesync: health POST error:", e, flush=True)


async def run():
    global loop, manager
    loop = asyncio.get_running_loop()
    m = None
    async with aiohttp.ClientSession() as session:
        while True:
            try:
                if m is None:
                    m = VeSync(USER, PASS)
                    if not await aw(m.login()):
                        raise RuntimeError("VeSync login failed")
                    manager = m
                    print("vesync: logged in, account", m.account_id, flush=True)
                n = await publish_devices(m)
                await publish_scale(m, session)
                print(f"vesync: published {n} devices", flush=True)
            except Exception as e:
                print("vesync: error:", e, flush=True)
                m = None
                manager = None
            await asyncio.sleep(POLL)


if __name__ == "__main__":
    asyncio.run(run())
