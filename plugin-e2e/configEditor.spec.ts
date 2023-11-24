import { expect, test } from '@grafana/plugin-e2e';

test('invalid credentials should return an error', async ({ createDataSourceConfigPage, page }) => {
  const configPage = await createDataSourceConfigPage({ type: 'cloudwatch' });
  await expect(configPage.saveAndTest()).not.toBeOK();
});
