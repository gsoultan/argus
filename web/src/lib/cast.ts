/**
 * Generates a realistic asciicast v2 recording for a session, so the replay
 * player exercises the real decode path rather than a toy fixture.
 *
 * Replace with `GET /api/v1/sessions/:id/recording` against object storage.
 */

/** Built at runtime so no raw control byte lives in the source file. */
const CSI = `${String.fromCharCode(27)}[`
const wrap = (code: string) => (s: string) => `${CSI}${code}m${s}${CSI}0m`
const dim = wrap('2')
const green = wrap('32')
const red = wrap('31')
const yellow = wrap('33')
const cyan = wrap('36')
const bold = wrap('1')

interface Step {
  cmd: string
  out: string[]
  think?: number
}

function script(host: string): Step[] {
  const short = host.split('.')[0]!
  return [
    {
      cmd: 'systemctl status payments-worker --no-pager',
      out: [
        `${green('*')} payments-worker.service - Northwind Payments Settlement Worker`,
        '     Loaded: loaded (/etc/systemd/system/payments-worker.service; enabled)',
        `     Active: ${green('active (running)')} since Thu 2026-08-27 22:14:03 WIB; 11h ago`,
        '   Main PID: 2214 (payments-worker)',
        '      Tasks: 34 (limit: 18841)',
        '     Memory: 1.4G (peak: 2.1G)',
        '        CPU: 6h 12min 44.019s',
        '',
      ],
    },
    {
      cmd: 'journalctl -u payments-worker -n 12 --no-pager',
      think: 900,
      out: [
        `${dim('Aug 28 09:31:02')} ${short} payments-worker[2214]: settle.batch id=B-88431 size=412 ok`,
        `${dim('Aug 28 09:31:44')} ${short} payments-worker[2214]: settle.batch id=B-88432 size=388 ok`,
        `${dim('Aug 28 09:32:19')} ${short} payments-worker[2214]: ${yellow('WARN')} upstream latency p99=2841ms`,
        `${dim('Aug 28 09:32:51')} ${short} payments-worker[2214]: ${red('ERROR')} settle.batch id=B-88433 timeout after 30s`,
        `${dim('Aug 28 09:32:51')} ${short} payments-worker[2214]: retry.enqueue id=B-88433 attempt=1`,
        `${dim('Aug 28 09:33:23')} ${short} payments-worker[2214]: ${red('ERROR')} settle.batch id=B-88433 timeout after 30s`,
        `${dim('Aug 28 09:33:23')} ${short} payments-worker[2214]: retry.enqueue id=B-88433 attempt=2`,
        `${dim('Aug 28 09:33:55')} ${short} payments-worker[2214]: ${red('ERROR')} settle.batch id=B-88433 timeout after 30s`,
        `${dim('Aug 28 09:33:55')} ${short} payments-worker[2214]: retry.enqueue id=B-88433 attempt=3`,
        `${dim('Aug 28 09:34:27')} ${short} payments-worker[2214]: ${red('ERROR')} settle.batch id=B-88433 dead-letter`,
        `${dim('Aug 28 09:35:01')} ${short} payments-worker[2214]: queue.depth=1841 ${yellow('growing')}`,
        `${dim('Aug 28 09:36:12')} ${short} payments-worker[2214]: queue.depth=2033 ${yellow('growing')}`,
        '',
      ],
    },
    { cmd: 'ss -tnp state established | grep -c 5432', think: 700, out: ['184', ''] },
    {
      cmd: 'df -h /var/lib/payments',
      out: [
        'Filesystem      Size  Used Avail Use% Mounted on',
        `/dev/nvme1n1    512G  ${yellow('471G')}   41G  ${yellow('93%')} /var/lib/payments`,
        '',
      ],
    },
    {
      cmd: 'sudo -n redis-cli -h 10.42.3.11 LLEN settle:retry',
      think: 1200,
      out: [`(integer) ${bold('2033')}`, ''],
    },
    {
      cmd: 'curl -s -o /dev/null -w "%{http_code} %{time_total}s" https://acquirer.internal/health',
      think: 800,
      out: [`${red('504')} 30.004s`, ''],
    },
    {
      cmd: 'echo "upstream acquirer is down, not us" | tee /tmp/INC-4471-note',
      out: ['upstream acquirer is down, not us', ''],
    },
    {
      cmd: 'logger -t argus "INC-4471 root cause: acquirer.internal 504, queue backpressure expected"',
      out: [''],
    },
    { cmd: 'exit', think: 400, out: [cyan('logout'), ''] },
  ]
}

/** Builds a newline-delimited asciicast v2 document. */
export function buildCast(host: string, principal: string, startedAt: string): string {
  const rows: string[] = []
  rows.push(
    JSON.stringify({
      version: 2,
      width: 120,
      height: 34,
      timestamp: Math.floor(Date.parse(startedAt) / 1000),
      title: `${principal}@${host}`,
      env: { SHELL: '/bin/bash', TERM: 'xterm-256color' },
    }),
  )

  let t = 0
  const emit = (data: string) => rows.push(JSON.stringify([Number(t.toFixed(3)), 'o', data]))
  const typed = (data: string) => rows.push(JSON.stringify([Number(t.toFixed(3)), 'i', data]))

  const prompt = `${green(`${principal}@${host.split('.')[0]}`)}:${cyan('~')}$ `

  emit(`Last login: Thu Aug 28 09:12:41 2026 from 10.0.0.9 ${dim('(brokered by argus-gw-01)')}\r\n`)
  t += 0.4
  emit(`${dim('-- session recorded - asciicast v2 - eBPF command audit active --')}\r\n\r\n`)
  t += 0.6

  for (const step of script(host)) {
    emit(prompt)
    t += 0.25

    // Character-by-character typing, so the replay has realistic frame density.
    for (const ch of step.cmd) {
      typed(ch)
      emit(ch)
      t += 0.03 + Math.random() * 0.05
    }
    // The Enter keypress is a real stdin event, and it is what terminates a
    // command line. Without it a consumer accumulating stdin sees the whole
    // session as one unbroken command.
    typed('\r')
    emit('\r\n')
    t += (step.think ?? 300) / 1000

    for (const line of step.out) {
      emit(`${line}\r\n`)
      t += 0.04 + Math.random() * 0.06
    }
    t += 0.5 + Math.random() * 1.5
  }

  return rows.join('\n')
}
