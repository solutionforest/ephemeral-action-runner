ObjC.import('Foundation');

function fail(message) {
  throw new Error(message);
}

function isData(value) {
  return value instanceof $.NSData;
}

function isDictionary(value) {
  if (value === null || typeof value !== 'object' || Array.isArray(value) || isData(value)) return false;
  const prototype = Object.getPrototypeOf(value);
  return prototype === Object.prototype || prototype === null;
}

function hasOwn(value, key) {
  return Object.prototype.hasOwnProperty.call(value, key);
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
  if (hasOwn(setting, 'kSecTrustSettingsPolicy')) {
    if (!isData(setting.kSecTrustSettingsPolicy) ||
        typeof setting.kSecTrustSettingsPolicyName !== 'string' ||
        setting.kSecTrustSettingsPolicyName !== 'sslServer') return false;
  } else if (hasOwn(setting, 'kSecTrustSettingsPolicyName')) {
    return false;
  }
  if (hasOwn(setting, 'kSecTrustSettingsKeyUsage')) {
    const usage = setting.kSecTrustSettingsKeyUsage;
    if (!Number.isInteger(usage) || ![-1, 0xffffffff, 0x1, 0x8].includes(usage)) return false;
  }
  return true;
}

function run(argv) {
  if (argv.length !== 1) fail('usage: macos-trust-settings.js <export.plist>');
  const data = $.NSData.dataWithContentsOfFile($(argv[0]));
  if (!data) fail('cannot read exported trust settings plist');
  const propertyList = $.NSPropertyListSerialization.propertyListWithDataOptionsFormatError(data, 0, null, null);
  if (!propertyList) fail('cannot parse exported trust settings plist');
  const document = ObjC.deepUnwrap(propertyList);
  if (!isDictionary(document) || document.trustVersion !== 1 || !isDictionary(document.trustList)) {
    fail('unsupported macOS trust settings document');
  }

  const lines = [];
  for (const [rawFingerprint, entry] of Object.entries(document.trustList)) {
    const fingerprint = rawFingerprint.trim().toUpperCase();
    if (!/^[0-9A-F]{40}$/.test(fingerprint)) fail('unsupported non-certificate/default trust entry: ' + rawFingerprint);
    if (!isDictionary(entry)) fail('trust entry must be a dictionary for ' + fingerprint);
    if (!hasOwn(entry, 'trustSettings')) {
      lines.push('allow ' + fingerprint);
      continue;
    }
    const settings = entry.trustSettings;
    if (!Array.isArray(settings)) fail('trustSettings must be an array for ' + fingerprint);

    let allow = settings.length === 0;
    let deny = false;
    for (const setting of settings) {
      if (!isDictionary(setting)) fail('trust setting must be an object for ' + fingerprint);
      const result = hasOwn(setting, 'kSecTrustSettingsResult') ? setting.kSecTrustSettingsResult : 1;
      if (!Number.isInteger(result) || ![0, 1, 2, 3, 4].includes(result)) fail('invalid trust result for ' + fingerprint);
      if (result === 3) deny = true;
      if ((result === 1 || result === 2) && positiveSettingIsExportable(setting)) allow = true;
    }
    if (deny) lines.push('deny ' + fingerprint);
    else if (allow) lines.push('allow ' + fingerprint);
  }
  return lines.join('\n');
}
