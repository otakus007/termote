import { expect, type Page, test } from './fixtures'

const waitForTerminal = (page: Page, timeout = 10000) =>
  page.waitForSelector('[data-testid="terminal-view"] .xterm', { timeout })

const openMenu = (page: Page) =>
  page.getByRole('button', { name: 'More' }).click()

const openSettingsGroup = async (page: Page, group: string) => {
  await openMenu(page)
  await page.getByRole('menuitem', { name: 'Settings' }).click()
  await page
    .getByRole('navigation', { name: 'Settings groups' })
    .getByRole('button', { name: group })
    .click()
}

const reloadFresh = async (page: Page) => {
  await page.goto('/')
  await page.evaluate(() => localStorage.clear())
  await page.reload()
  await waitForTerminal(page)
}

test.describe('2D keys board & mobile UX', () => {
  test.beforeEach(async ({ page }) => {
    await reloadFresh(page)
  })

  test('switches presets, exports and imports tmk1: share codes, and persists layout', async ({
    page,
  }) => {
    await openSettingsGroup(page, 'Terminal')
    await page.getByRole('button', { name: 'Customize keys…' }).click()

    const dialog = page.getByRole('dialog', { name: 'Customize Keys Board' })
    await expect(dialog).toBeVisible()

    // Switch to Vim preset and inspect exported tmk1: code
    await dialog.getByRole('button', { name: 'Vim' }).click()
    const shareInput = dialog.getByTestId('keys-share-code')
    const vimCode = await shareInput.inputValue()
    expect(vimCode.startsWith('tmk1:')).toBe(true)

    // Switch back to Default, then import the Vim tmk1: code
    await dialog.getByRole('button', { name: 'Default' }).click()
    const importInput = dialog.getByRole('textbox', {
      name: 'Import layout code',
    })
    await importInput.fill('tmk1:invalid-payload')
    await dialog.getByRole('button', { name: 'Import' }).click()
    await expect(dialog.getByRole('alert')).toContainText(
      'Invalid or unsupported tmk1: layout code',
    )

    await importInput.fill(vimCode)
    await dialog.getByRole('button', { name: 'Import' }).click()
    await expect(dialog.getByRole('button', { name: 'Vim' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )

    await dialog.getByRole('button', { name: 'Save layout' }).click()
    await expect(dialog).toBeHidden()

    const saved = await page.evaluate(() =>
      JSON.parse(localStorage.getItem('termote-settings') ?? '{}'),
    )
    expect(saved.keysLayout?.preset).toBe('vim')
  })

  test('toggles left-handed mode and Zen mode from Terminal settings', async ({
    page,
  }) => {
    await openSettingsGroup(page, 'Terminal')

    await page.getByRole('switch', { name: 'Left-handed mode' }).click()
    await expect(page.locator('[data-left-handed="true"]').first()).toBeVisible()

    await page.getByRole('switch', { name: 'Zen mode' }).click()
    await page.keyboard.press('Escape')

    const exitZenBtn = page.getByTestId('exit-zen-mode')
    await expect(exitZenBtn).toBeVisible()

    await exitZenBtn.click()
    await expect(exitZenBtn).toBeHidden()
  })
})
