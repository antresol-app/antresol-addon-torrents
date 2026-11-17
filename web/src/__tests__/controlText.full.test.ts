// The full build's control labels centre on their capitals too
// (--control-text-box), like the core build's (controlText.test.ts).
import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

/** The body of `selector`'s rule in a CSS file of the addon's pages. */
function rule(file: string, selector: string): string {
  const css = readFileSync(join(dirname(fileURLToPath(import.meta.url)), '..', 'pages', file), 'utf8')
  const at = css.indexOf(`${selector} {`)
  return at < 0 ? '' : css.slice(at, css.indexOf('}', at))
}

describe('control labels (full build)', () => {
  it('the sources form’s «Advanced» disclosure centres on its capitals', () => {
    expect(rule('Sources.module.css', '.advancedLabel')).toContain('text-box: var(--control-text-box)')
  })
})
