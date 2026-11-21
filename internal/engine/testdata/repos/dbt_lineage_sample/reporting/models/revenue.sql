select * from {{ source('warehouse', 'orders_daily') }}
