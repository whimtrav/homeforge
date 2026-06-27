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

export function connectWS(onMessage: (msg: WSMessage) => void): () => void {
  const wsBase = import.meta.env.DEV ? 'ws://localhost:8123' : `ws://${location.host}`
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
