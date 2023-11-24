import { expect, test } from '@grafana/plugin-e2e';

export type RedshiftDatasourceConfig = {
  name: string;
};

test('should return data and not display panel error when a valid query is provided', async ({
  panelEditPage,
  page,
  readProvision,
}) => {
  await panelEditPage.datasource.set('gdev-cloudwatch');
  const queryEditorRow = await panelEditPage.getQueryEditorRow('A');
  await queryEditorRow.locator('[aria-label="Namespace"]').fill('AWS/EC2');
  await page.keyboard.press('Enter');
  await queryEditorRow.locator('[aria-label="Metric name"]').fill('CPUUtilization');
  await page.keyboard.press('Enter');
  await queryEditorRow.getByLabel('Toggle switch').click();
  await expect(panelEditPage.refreshPanel()).toBeOK();
  await expect(panelEditPage).not.toHavePanelError();
});
