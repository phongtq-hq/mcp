import { useEffect, useRef, useState } from 'react';
import { durationMs, reducedMotion } from './motion';

// useTween: một con số đi dần tới giá trị mới thay vì nhảy, để người dùng thấy số vừa đổi
// (và đổi theo chiều nào) chứ không tưởng trang vừa tải lại. Lần hiện đầu tiên không có
// chuyển động: chưa có giá trị cũ nào để so. Giá trị trả về ở khung cuối luôn đúng bằng đích.
export function useTween(target: number): number {
  const [shown, setShown] = useState(target);
  const from = useRef(target);
  useEffect(() => {
    const start = from.current;
    from.current = target;
    if (start === target || !Number.isFinite(start) || !Number.isFinite(target)) { setShown(target); return; }
    if (reducedMotion() || typeof requestAnimationFrame !== 'function') { setShown(target); return; }
    const total = durationMs('slow') * 2;
    const t0 = performance.now();
    let frame = 0;
    const tick = (now: number) => {
      const p = Math.min(1, (now - t0) / total);
      const eased = 1 - (1 - p) ** 3;
      setShown(p === 1 ? target : start + (target - start) * eased);
      if (p < 1) frame = requestAnimationFrame(tick);
    };
    frame = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(frame);
  }, [target]);
  return shown;
}
