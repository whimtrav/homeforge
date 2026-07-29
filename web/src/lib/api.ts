export interface Entity {
  id: string
  name: string
  domain: string
  state: string
  attributes: Record<string, any>
  last_changed: string
  last_updated: string
}

export interface WSMessage {
  type: 'snapshot' | 'state_changed'
  entity?: Entity
  entities?: Entity[]
}

const base = import.meta.env.DEV ? 'http://localhost:8123' : ''

export async function fetchEntities(): Promise<Entity[]> {
  const r = await fetch(`${base}/api/entities`)
  return r.json()
}

export async function callService(
  domain: string,
  service: string,
  entityId: string,
  data: Record<string, any> = {}
): Promise<void> {
  await fetch(`${base}/api/services/${domain}/${service}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ entity_id: entityId, data }),
  })
}

export interface Automation {
  name: string
  enabled: boolean
  trigger: string
  actions: string[]
}

export async function fetchAutomations(): Promise<Automation[]> {
  const r = await fetch(`${base}/api/automations`)
  return r.json()
}

export async function setAutomationEnabled(name: string, enabled: boolean): Promise<void> {
  await fetch(`${base}/api/automations/${encodeURIComponent(name)}/enabled`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ enabled }),
  })
}

export function connectWS(onMessage: (msg: WSMessage) => void): () => void {
  // Match the WS scheme to the page: wss:// on HTTPS (e.g. via the Cloudflare tunnel), else ws://.
  // Using ws:// on an https page is mixed content and the browser blocks it → app hangs "connecting".
  const wsProto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  const wsBase = import.meta.env.DEV ? 'ws://localhost:8123' : `${wsProto}//${location.host}`
  const ws = new WebSocket(`${wsBase}/api/ws`)

  ws.onmessage = (ev) => {
    try {
      onMessage(JSON.parse(ev.data))
    } catch {}
  }

  ws.onclose = () => {
    setTimeout(() => connectWS(onMessage), 2000)
  }

  return () => ws.close()
}

export function entityIcon(domain: string): string {
  const icons: Record<string, string> = {
    light: '💡',
    switch: '🔌',
    binary_sensor: '👁',
    sensor: '📊',
    lock: '🔒',
    climate: '🌡',
    alarm_control_panel: '🚨',
    camera: '📷',
  }
  return icons[domain] ?? '⚙️'
}

// Relays whose pin is named light/ceiling but which are actually POWER FEEDS to smart
// bulbs (the real light is a WiZ bulb). Metadata can't tell these from a real
// ceiling-light relay, so they're listed explicitly. Add new ones here as found.
const POWER_FEED_RELAYS = new Set(['hallway-ceiling', 'upbath-light'])

// deviceIcon classifies a whole DEVICE by name + pin_name (pin_type is unreliable —
// a presence bool and a Sonoff onboard button both report pin_type "relay").
// Shared by the Devices grid and the Floor Plan so their icons never drift.
//   🌀 fan · 🎨 WLED · 🎛️ switch · 📡 sensor · 💡 light · 🔌 relay
export function deviceIcon(name: string, entities: Entity[]): string {
  const nameL = (name || '').toLowerCase()
  if (POWER_FEED_RELAYS.has(nameL)) return '🔌'
  const attrs = entities.map((e) => e.attributes || {})
  const pinNames = new Set<string>(attrs.map((a) => a.pin_name).filter(Boolean))
  const ids = entities.map((e) => e.id)
  const hasWiz = attrs.some((a) => a.wiz_mac)
  const hasWled = attrs.some((a) => a.wled_host)
  const pn = (...names: string[]) => names.some((n) => pinNames.has(n))
  const anyPin = (re: RegExp) => [...pinNames].some((n) => re.test(n))
  const isFan = entities.some((e) => e.domain === 'number' && (e.attributes?.pin_name === 'fan' || e.id.endsWith('_fan')))

  if (hasWled || /-wled$/.test(nameL)) return '🎨'
  if (isFan || pn('fan') || ids.some((i) => /_fan$/.test(i)) || /(^|[-_])fan([-_]|$)/.test(nameL)) return '🌀'
  if (/-switch$/.test(nameL) || pn('touchpad') || anyPin(/pad$/)) return '🎛️'
  const lightish =
    hasWiz || pn('neolight') ||
    /(ceiling|soffit|lamp|mirror|shower|closet|sink|downlight|downstairs|vanity|sconce|-lights$)/.test(nameL)
  const relayish = pn('relay', 'outlet', 'power1', 'power2') || attrs.some((a) => a.bl0939)
  if (anyPin(/^(presence|radar|climate|motion|air|pir|temp|humidity|co2|tvoc)/) && !lightish && !relayish) return '📡'
  if (lightish) return '💡'
  if (relayish || entities.some((e) => e.domain === 'switch')) return '🔌'
  return '📟'
}

export function isOn(entity: Entity): boolean {
  return entity.state === 'on' || entity.state === 'ON'
}

export function groupByDomain(entities: Entity[]): Map<string, Entity[]> {
  const map = new Map<string, Entity[]>()
  for (const e of entities) {
    const group = map.get(e.domain) ?? []
    group.push(e)
    map.set(e.domain, group)
  }
  return map
}
