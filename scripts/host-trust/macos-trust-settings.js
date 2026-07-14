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

function isStandardBase64(value) {
  if (value === '') return true;
  return /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(value);
}

function validateXMLDataElements(xml) {
  const openingCount = (xml.match(/<data(?=[\s/>])/g) || []).length;
  const element = /<data>([\s\S]*?)<\/data>|<data\s*\/>/g;
  let validatedCount = 0;
  let match;
  while ((match = element.exec(xml)) !== null) {
    const encoded = (match[1] || '').replace(/[ \t\r\n]/g, '');
    if (!isStandardBase64(encoded)) fail('plist data must contain standard base64');
    validatedCount++;
  }
  if (validatedCount !== openingCount) fail('plist data elements must be attribute-free and well formed');
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
  const source = $.NSString.alloc.initWithDataEncoding(data, $.NSUTF8StringEncoding);
  if (!source) fail('exported trust settings plist must be UTF-8 XML');
  const xml = ObjC.unwrap(source);
  const trimmedXML = xml.replace(/^[ \t\r\n]+/, '');
  if (!trimmedXML.startsWith('<?xml') && !trimmedXML.startsWith('<plist')) fail('exported trust settings plist must be XML');
  validateXMLDataElements(xml);
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
