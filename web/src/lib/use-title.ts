import { useEffect } from 'react';

const APP = 'MCP Platform';

// useTitle: tiêu đề tab trình duyệt theo màn hình đang mở ("Chi phí · MCP Platform"), để
// phân biệt được các tab. Rời màn hình thì trả về tên ứng dụng.
export function useTitle(title: string | null | undefined): void {
  useEffect(() => {
    if (!title) return;
    document.title = title + ' · ' + APP;
    return () => { document.title = APP; };
  }, [title]);
}
