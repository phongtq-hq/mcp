// Bộ biểu tượng của giao diện: nét mảnh trên lưới 24, vẽ bằng SVG ngay trong mã nguồn. Không
// dùng gói biểu tượng: vài chục đường nét không đáng một phụ thuộc lúc chạy (ADR-0016).
// Biểu tượng chỉ để trang trí: luôn đi kèm chữ hoặc nhãn aria ở phần tử chứa nó.
const PATHS = {
  home: 'M3 11l9-7 9 7M5 10v10h5v-6h4v6h5V10',
  bot: 'M12 3v4M7 7h10a3 3 0 0 1 3 3v6a3 3 0 0 1-3 3H7a3 3 0 0 1-3-3v-6a3 3 0 0 1 3-3zM9 12v2M15 12v2M2 13v2M22 13v2',
  map: 'M6 3v6M6 15v6M6 9a3 3 0 1 0 0 6 3 3 0 0 0 0-6zM18 6a3 3 0 1 0 0 6 3 3 0 0 0 0-6zM9 12h3a3 3 0 0 0 3-3',
  folder: 'M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z',
  server: 'M4 5h16v6H4zM4 13h16v6H4zM8 8h.01M8 16h.01',
  tool: 'M14.5 6.5a4 4 0 0 0-5.2 5.2L3 18l3 3 6.3-6.3a4 4 0 0 0 5.2-5.2L15 12l-3-3z',
  cpu: 'M7 7h10v10H7zM10 3v4M14 3v4M10 17v4M14 17v4M3 10h4M3 14h4M17 10h4M17 14h4',
  shield: 'M12 3l8 3v6c0 4.5-3.2 7.8-8 9-4.800-1.200-8-4.500-8-9V6z',
  check: 'M4 12.5l5 5L20 6.500',
  list: 'M8 6h12M8 12h12M8 18h12M4 6h.01M4 12h.01M4 18h.01',
  coins: 'M9 14a6 3 0 1 0 12 0 6 3 0 0 0-12 0zM9 14v3c0 1.700 2.700 3 6 3s6-1.300 6-3v-3M3 7a6 3 0 1 0 12 0A6 3 0 0 0 3 7zM3 7v6c0 1.200 1.400 2.300 3.500 2.800',
  building: 'M5 21V4h9v17M14 9h5v12M3 21h18M8 8h3M8 12h3M8 16h3',
  search: 'M11 4a7 7 0 1 0 0 14 7 7 0 0 0 0-14zM20 20l-4-4',
  left: 'M15 5l-7 7 7 7',
  right: 'M9 5l7 7-7 7',
  down: 'M5 9l7 7 7-7',
  close: 'M5 5l14 14M19 5L5 19',
  menu: 'M4 6h16M4 12h16M4 18h16',
  sun: 'M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8zM12 2v2M12 20v2M4.900 4.900l1.400 1.400M17.700 17.700l1.400 1.400M2 12h2M20 12h2M4.900 19.100l1.400-1.400M17.700 6.300l1.400-1.400',
  moon: 'M20 14.500A8 8 0 0 1 9.500 4a8 8 0 1 0 10.500 10.500z',
  filter: 'M3 5h18l-7 8v6l-4 2v-8z',
  fit: 'M4 9V4h5M20 9V4h-5M4 15v5h5M20 15v5h-5',
  plus: 'M12 5v14M5 12h14',
  minus: 'M5 12h14',
  hand: 'M8 13V5.500a1.500 1.500 0 0 1 3 0V11M11 11V4.500a1.500 1.500 0 0 1 3 0V11M14 11V6.500a1.500 1.500 0 0 1 3 0V14c0 4-2 7-6 7s-5-2-7-6l-1-2.500a1.400 1.400 0 0 1 2.500-1.200L8 13',
  pointer: 'M5 3l14 7-6 2-2 6z',
  layers: 'M12 3l9 5-9 5-9-5zM3 13l9 5 9-5',
  globe: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM3 12h18M12 3c2.500 2.500 3.500 5.500 3.500 9s-1 6.500-3.500 9c-2.500-2.500-3.500-5.500-3.500-9s1-6.500 3.500-9z',
  lock: 'M6 11h12v9H6zM9 11V8a3 3 0 0 1 6 0v3',
  alert: 'M12 4l9 16H3zM12 10v4M12 17h.01',
  refresh: 'M20 12a8 8 0 1 1-2.300-5.700M20 4v5h-5',
  bell: 'M6 16V11a6 6 0 0 1 12 0v5l2 2H4zM10 21h4',
  user: 'M12 4a4 4 0 1 0 0 8 4 4 0 0 0 0-8zM4 21c0-4 3.600-6 8-6s8 2 8 6',
  play: 'M7 4l12 8-12 8z',
  eye: 'M2 12c2.500-4.500 6-7 10-7s7.500 2.500 10 7c-2.500 4.500-6 7-10 7s-7.500-2.500-10-7zM12 9a3 3 0 1 0 0 6 3 3 0 0 0 0-6z',
  sidebar: 'M4 5h16v14H4zM9 5v14',
  expand: 'M8 4H4v4M16 4h4v4M8 20H4v-4M16 20h4v-4M9 9L4 4M15 9l5-5M9 15l-5 5M15 15l5 5',
} as const;

export type IconName = keyof typeof PATHS;

export function Icon({ name, size = 16 }: { name: IconName; size?: number }) {
  return (
    <svg className="icon" width={size} height={size} viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor"
      strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
      <path d={PATHS[name]} />
    </svg>
  );
}
