import { render, screen } from '@testing-library/react'
import { expect, it } from 'vitest'
import { recommendation } from '../test/overviewFixture'
import { CompactGameIdentity } from './CompactGameIdentity'

it.each([
  'javascript:alert(1)', 'http://store.steampowered.com/app/480/', 'https://store.steampowered.com.evil/app/480/',
  'https://store.steampowered.com/app/481/', 'https://store.steampowered.com/app/480/?next=evil', 'https://evil@store.steampowered.com/app/480/', '',
])('does not turn untrusted or noncanonical server URL %s into an anchor', (storeUrl) => {
  render(<CompactGameIdentity item={{ ...recommendation('Длинное название LongName'.repeat(8)), storeUrl }} size="list" />)
  expect(screen.queryByRole('link')).not.toBeInTheDocument()
  expect(document.querySelector('.game-poster')).not.toBeNull()
})
it.each([0, -1, 1.2, NaN, Infinity])('does not link a non-positive or invalid AppID %s', (steamAppId) => {
  render(<CompactGameIdentity item={{ ...recommendation('Game'), steamAppId, storeUrl: `https://store.steampowered.com/app/${steamAppId}/` }} size="grid" />)
  expect(screen.queryByRole('link')).not.toBeInTheDocument()
})
