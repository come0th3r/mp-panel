import { lazy, Suspense } from 'react';
import { Loading } from './ui';
const MetricChart = lazy(() => import('./Metrics').then(module => ({ default: module.Metrics })));
export function Metrics({ path }: { path: string }) { return <Suspense fallback={<Loading />}><MetricChart path={path} /></Suspense>; }
