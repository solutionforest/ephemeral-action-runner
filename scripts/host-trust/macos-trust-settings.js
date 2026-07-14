ObjC.import('Foundation');

function fail(message) {
  throw new Error(message);
}

function positiveSettingIsExportable(setting) {
  const allowed = new Set([
    'kSecTrustSettingsPolicy',
    'kSecTrustSettingsPolicyName',
    'kSecTrustSettingsKeyUsage',
    'kSecTrustSettingsResult'
  ]);
  for (const key of Object.keys(setting)) {
    if (!allowed.has(key)) return false;
  }
  if (Object.prototype.hasOwnProperty.call(setting, 'kSecTrustSettingsPolicy')) {
    if (setting.kSecTrustSettingsPolicyName !== 'sslServer') return false;
  } else if (Object.prototype.hasOwnProperty.call(setting, 'kSecTrustSettingsPolicyName')) {
    return false;
  }
  if (Object.prototype.hasOwnProperty.call(setting, 'kSecTrustSettingsKeyUsage')) {
    const usage = setting.kSecTrustSettingsKeyUsage;
    if (!Number.isInteger(usage) || ![-1, 0xffffffff, 0x1, 0x8].includes(usage)) return false;
  }
  return true;
}

function run(argv) {
  if (argv.length !== 1) fail('usage: macos-trust-settings.js <export.json>');
  const value = $.NSString.stringWithContentsOfFileEncodingError(
    $(argv[0]), $.NSUTF8StringEncoding, null
  );
  if (!value) fail('cannot read exported trust settings JSON');
  const document = JSON.parse(ObjC.unwrap(value));
  if (document.trustVersion !== 1 || !document.trustList ||
      typeof document.trustList !== 'object' || Array.isArray(document.trustList)) {
    fail('unsupported macOS trust settings document');
  }
  const lines = [];
  for (const [rawFingerprint, entry] of Object.entries(document.trustList)) {
    const fingerprint = rawFingerprint.trim().toUpperCase();
    if (!/^[0-9A-F]{40}$/.test(fingerprint)) {
      fail('unsupported non-certificate/default trust entry: ' + rawFingerprint);
    }
    if (!entry || !Object.prototype.hasOwnProperty.call(entry, 'trustSettings') ||
        !Array.isArray(entry.trustSettings)) {
      fail('trustSettings must be a present array for ' + fingerprint);
    }
    const settings = entry.trustSettings;
    let allow = settings.length === 0;
    let deny = false;
    for (const setting of settings) {
      if (!setting || typeof setting !== 'object' || Array.isArray(setting)) {
        fail('trust setting must be an object for ' + fingerprint);
      }
      const result = Object.prototype.hasOwnProperty.call(setting, 'kSecTrustSettingsResult')
        ? setting.kSecTrustSettingsResult : 1;
      if (![0, 1, 2, 3, 4].includes(result)) fail('invalid trust result for ' + fingerprint);
      if (result === 3) deny = true;
      if ((result === 1 || result === 2) && positiveSettingIsExportable(setting)) allow = true;
    }
    if (deny) lines.push('deny ' + fingerprint);
    else if (allow) lines.push('allow ' + fingerprint);
  }
  return lines.join('\n');
}
