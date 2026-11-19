import js from '@eslint/js';
import reactHooks from 'eslint-plugin-react-hooks';
import globals from 'globals';
import tseslint from 'typescript-eslint';

// Dữ liệu từ API (kể cả nội dung do model hay hệ thống ngoài sinh ra) chỉ vào trang dưới
// dạng text node của React. Các lối đưa HTML thô vào DOM bị cấm (security.md §4.1).
const unsafeDom = 'Không đưa HTML thô vào DOM: dữ liệu chỉ vào trang dưới dạng văn bản.';
const HTML_SINK = '/^(innerHTML|outerHTML|insertAdjacentHTML|setHTMLUnsafe|parseFromString|createContextualFragment|srcdoc)$/';
const noUnsafeDom = [
  { selector: "JSXAttribute[name.name='dangerouslySetInnerHTML']", message: unsafeDom },
  { selector: "Property[key.name='dangerouslySetInnerHTML']", message: unsafeDom },
  { selector: `MemberExpression[property.name=${HTML_SINK}]`, message: unsafeDom },
  // Truy cập bằng chuỗi (el['innerHTML']) cũng bị cấm.
  { selector: `MemberExpression[computed=true][property.value=${HTML_SINK}]`, message: unsafeDom },
  { selector: "CallExpression[callee.object.name='document'][callee.property.name=/^write(ln)?$/]", message: unsafeDom },
  { selector: "NewExpression[callee.name='Function']", message: 'Không dựng code từ chuỗi.' },
];
// import() động chỉ dùng ở bảng route (app/): ở nơi khác nó lách luật phụ thuộc bên dưới.
const noDynamicImport = { selector: 'ImportExpression', message: 'import() động chỉ dùng ở src/app (bảng route).' };

// Một chỗ duy nhất gọi mạng (api/ tới /v1, auth/ tới IdP): token, Idempotency-Key và xử lý
// lỗi không bị lặp lại ở từng màn hình.
const network = 'Gọi mạng chỉ ở src/api và src/auth; nơi khác dùng @/api/client.';
const noNetworkGlobals = ['error', ...['fetch', 'XMLHttpRequest', 'EventSource', 'WebSocket'].map(name => ({ name, message: network }))];
const noNetworkProperties = ['error',
  ...['window', 'globalThis', 'self'].flatMap(object => ['fetch', 'XMLHttpRequest', 'EventSource', 'WebSocket'].map(property => ({ object, property, message: network }))),
  { object: 'navigator', property: 'sendBeacon', message: network },
];

// Luật phụ thuộc giữa các tầng, tương tự depguard phía Go. Import ra ngoài thư mục của
// mình phải dùng bí danh @/ (import tương đối `../` bị cấm), nên các mẫu theo @/ bên dưới
// không lách được bằng đường dẫn tương đối.
const relative = { group: ['../**', '..'], message: 'Import ngoài thư mục hiện tại dùng bí danh @/.' };
const layer = (message, ...groups) => ['error', { patterns: [relative, { group: groups, message }] }];

export default tseslint.config(
  { ignores: ['node_modules', 'src/api/schema.gen.ts'] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    files: ['src/**/*.{ts,tsx}'],
    languageOptions: { globals: globals.browser },
    plugins: { 'react-hooks': reactHooks },
    rules: {
      'react-hooks/rules-of-hooks': 'error',
      'react-hooks/exhaustive-deps': 'error',
      'no-eval': 'error',
      'no-implied-eval': 'error',
      'no-restricted-syntax': ['error', ...noUnsafeDom, noDynamicImport],
      'no-restricted-globals': noNetworkGlobals,
      'no-restricted-properties': noNetworkProperties,
      'no-restricted-imports': ['error', { patterns: [relative] }],
    },
  },
  { files: ['src/app/**'], rules: { 'no-restricted-syntax': ['error', ...noUnsafeDom] } },
  { files: ['src/api/**', 'src/auth/**'], rules: { 'no-restricted-globals': 'off', 'no-restricted-properties': 'off' } },
  {
    files: ['src/features/**'],
    rules: { 'no-restricted-imports': layer('Feature không import feature khác hay khung ứng dụng; đưa phần dùng chung xuống components/ hoặc lib/.', '@/features/**', '@/app/**') },
  },
  {
    files: ['src/lib/**', 'src/i18n/**'],
    rules: { 'no-restricted-imports': layer('lib/ và i18n/ là tầng thấp nhất.', '@/features/**', '@/components/**', '@/api/**', '@/auth/**', '@/app/**') },
  },
  {
    files: ['src/components/**'],
    rules: { 'no-restricted-imports': layer('components/ chỉ dùng lib/ và i18n/.', '@/features/**', '@/api/**', '@/auth/**', '@/app/**') },
  },
  {
    files: ['src/auth/**'],
    rules: { 'no-restricted-imports': layer('auth/ chỉ dùng lib/ và i18n/.', '@/features/**', '@/components/**', '@/api/**', '@/app/**') },
  },
  {
    files: ['src/api/**'],
    rules: { 'no-restricted-imports': layer('api/ không phụ thuộc vào màn hình.', '@/features/**', '@/components/**', '@/app/**') },
  },
  { files: ['*.config.{js,ts}'], languageOptions: { globals: globals.node } },
);
