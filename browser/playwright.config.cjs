const {defineConfig} = require("@playwright/test");

module.exports = defineConfig({
  testDir: ".",
  workers: 1,
  timeout: 30000,
  use: {
    browserName: "chromium",
    channel: process.env.BROWSER_CHANNEL || undefined,
  },
});
